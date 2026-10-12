package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
)

const (
	haKey          = "argus-ha"
	failoverAfter  = 2 * time.Minute
	heartbeatEvery = 10 * time.Second
)

type Protected struct {
	VM        string    `json:"vm"`
	Owner     string    `json:"owner"`
	Replica   string    `json:"replica"`
	Interval  int       `json:"interval"`
	LastSync  time.Time `json:"lastSync"`
	LastError string    `json:"lastError"`
}

func (c *Manager) Protected(ctx context.Context) ([]Protected, error) {
	ps, _, err := c.loadHA(ctx)
	return ps, err
}

func (c *Manager) loadHA(ctx context.Context) ([]Protected, string, error) {
	var ps []Protected
	rv, err := auth.LoadJSON(ctx, c.store, haKey, "vms", &ps)
	return ps, rv, err
}

func (c *Manager) updateHA(ctx context.Context, fn func([]Protected) ([]Protected, error)) error {
	c.haMu.Lock()
	defer c.haMu.Unlock()
	ps, rv, err := c.loadHA(ctx)
	if err != nil {
		return err
	}
	if ps, err = fn(ps); err != nil {
		return err
	}
	return auth.SaveJSON(ctx, c.store, haKey, "vms", rv, ps)
}

func vmPath(vm, suffix string) string {
	return APIPath("/api/vms/%s/%s/", kube.Namespace, vm) + suffix
}

func (c *Manager) Protect(ctx context.Context, owner, vm, replica string, interval int) error {
	if owner == replica {
		return errors.New("replica must be on another node")
	}
	o, err := c.Node(ctx, owner)
	if err != nil {
		return err
	}
	rn, err := c.Node(ctx, replica)
	if err != nil {
		return err
	}
	if rn.Org != o.Org {
		return errors.New("replica must be in the same organization")
	}
	if interval < 5 {
		return errors.New("interval must be at least 5 minutes")
	}
	err = c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		for _, p := range ps {
			if p.VM == vm && (p.Owner == owner || p.Replica == owner || p.Owner == replica || p.Replica == replica) {
				return nil, fmt.Errorf("%s is already protected", vm)
			}
		}
		return append(ps, Protected{VM: vm, Owner: owner, Replica: replica, Interval: interval}), nil
	})
	if err != nil {
		return err
	}
	c.heartbeat(ctx)
	return o.do(ctx, http.MethodPost, vmPath(vm, "ha"), map[string]bool{"enabled": true}, nil)
}

func (c *Manager) Unprotect(ctx context.Context, node, vm string) error {
	var p Protected
	err := c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		out := ps[:0]
		for _, x := range ps {
			if x.VM == vm && (x.Owner == node || x.Replica == node) {
				p = x
				continue
			}
			out = append(out, x)
		}
		if p.VM == "" {
			return nil, fmt.Errorf("%s is not protected", vm)
		}
		return out, nil
	})
	if err != nil {
		return err
	}
	if o, err := c.Node(ctx, p.Owner); err == nil {
		o.do(ctx, http.MethodPost, vmPath(vm, "ha"), map[string]bool{"enabled": false}, nil)
	}
	if r, err := c.Node(ctx, p.Replica); err == nil {
		return r.do(ctx, http.MethodPost, vmPath(vm, "abort"), map[string]string{"operation": kube.HAReplicaOperation}, nil)
	}
	return nil
}

func (c *Manager) Start() {
	c.started = time.Now()
	c.loadJobs(context.Background())
	go c.pollLoop()
	go func() {
		for range time.Tick(heartbeatEvery) {
			recovering("heartbeat", func() {
				ctx, cancel := context.WithTimeout(context.Background(), heartbeatEvery)
				defer cancel()
				c.heartbeat(ctx)
				c.failover(ctx)
			})
		}
	}()
	go func() {
		for range time.Tick(30 * time.Second) {
			recovering("replication", c.replicate)
		}
	}()
	go func() {
		for range time.Tick(drsEvery) {
			recovering("drs", func() { c.drsTick(context.Background()) })
		}
	}()
}

func (c *Manager) LastSeen(node string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen[node]
}

func (c *Manager) heartbeat(ctx context.Context) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return
	}
	ps, err := c.Protected(ctx)
	if err != nil {
		return
	}
	urls := map[string]string{}
	for _, n := range nodes {
		urls[n.Name] = n.URL
	}
	var wg sync.WaitGroup
	for _, n := range nodes {
		owned, replicas := []string{}, map[string]string{}
		for _, p := range ps {
			if p.Owner == n.Name {
				owned = append(owned, p.VM)
				replicas[p.VM] = urls[p.Replica]
			}
		}
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			if err := n.do(ctx, http.MethodPost, "/api/heartbeat", map[string]any{"owned": owned, "replicas": replicas}, nil); err == nil {
				c.mu.Lock()
				c.lastSeen[n.Name] = time.Now()
				c.mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
}

func (c *Manager) down(node string) bool {
	seen := c.LastSeen(node)
	if seen.IsZero() {
		seen = c.started
	}
	return time.Since(seen) > failoverAfter
}

const haSettingsKey = "argus-ha-settings"

type HASettings struct {
	Automatic bool `json:"automatic"`
}

func (c *Manager) HASettings(ctx context.Context) HASettings {
	var h HASettings
	auth.LoadJSON(ctx, c.store, haSettingsKey, "ha", &h)
	return h
}

func (c *Manager) SetHASettings(ctx context.Context, h HASettings) error {
	return auth.PutJSON(ctx, c.store, haSettingsKey, "ha", h)
}

func (c *Manager) Down(node string) bool { return c.down(node) }

func (c *Manager) ManualFailover(ctx context.Context, owner, vm string) error {
	ps, err := c.Protected(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ps, func(p Protected) bool { return p.VM == vm && p.Owner == owner })
	switch {
	case i < 0:
		return fmt.Errorf("%s is not protected on %s", vm, owner)
	case !c.down(owner):
		return fmt.Errorf("%s is still reachable; migrate instead", owner)
	case c.down(ps[i].Replica):
		return fmt.Errorf("replica host %s is down too", ps[i].Replica)
	case ps[i].LastSync.IsZero():
		return errors.New("the replica has no completed copy")
	}
	return c.promote(ctx, ps[i], "Failed over (manual)")
}

func (c *Manager) promote(ctx context.Context, p Protected, status string) error {
	owner, err1 := c.Node(ctx, p.Owner)
	replica, err2 := c.Node(ctx, p.Replica)
	if err1 != nil || err2 != nil {
		return errors.Join(err1, err2)
	}
	err := c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		for i := range ps {
			if ps[i].VM == p.VM && ps[i].Owner == p.Owner {
				ps[i].Owner, ps[i].Replica = ps[i].Replica, ps[i].Owner
				ps[i].LastSync = time.Time{}
			}
		}
		return ps, nil
	})
	if err != nil {
		return err
	}
	log.Printf("ha: %s %s from %s to %s", p.VM, strings.ToLower(status), owner.Name, replica.Name)
	c.mu.Lock()
	c.jobs = append(c.jobs, &Job{Org: owner.Org, VM: p.VM, From: owner.Name, To: replica.Name, Started: time.Now(), Status: status, Done: true})
	c.mu.Unlock()
	c.saveJobs()
	return nil
}

func (c *Manager) failover(ctx context.Context) {
	if !c.HASettings(ctx).Automatic {
		return
	}
	ps, err := c.Protected(ctx)
	if err != nil {
		return
	}
	for _, p := range ps {
		if !c.down(p.Owner) || c.down(p.Replica) || p.LastSync.IsZero() {
			continue
		}
		owner, err1 := c.Node(ctx, p.Owner)
		replica, err2 := c.Node(ctx, p.Replica)
		if err1 != nil || err2 != nil || owner.Maintenance {
			continue
		}
		var probe struct{ Reachable bool }
		if err := replica.do(ctx, http.MethodGet, "/api/probe?url="+url.QueryEscape(owner.URL), nil, &probe); err != nil || probe.Reachable {
			continue
		}
		c.promote(ctx, p, "Failed over (HA)")
	}
}

var replications = make(chan struct{}, 2)

func (c *Manager) replicate() {
	ctx := context.Background()
	ps, err := c.Protected(ctx)
	if err != nil {
		return
	}
	for _, p := range ps {
		if c.down(p.Owner) || c.down(p.Replica) || time.Since(p.LastSync) < time.Duration(p.Interval)*time.Minute {
			continue
		}
		c.mu.Lock()
		busy := c.syncing[p.VM] || c.busy[busyKey(p.Owner, p.VM)] || c.busy[busyKey(p.Replica, p.VM)]
		if !busy {
			c.syncing[p.VM] = true
		}
		c.mu.Unlock()
		if busy {
			continue
		}
		go func(p Protected) {
			defer func() {
				c.mu.Lock()
				delete(c.syncing, p.VM)
				c.mu.Unlock()
			}()
			sctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
			defer cancel()
			replications <- struct{}{}
			err := c.sync(sctx, p)
			<-replications
			msg := ""
			if err != nil {
				msg = err.Error()
				log.Printf("ha: replicate %s: %v", p.VM, err)
			}
			c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
				for i := range ps {
					if ps[i].VM == p.VM && ps[i].Owner == p.Owner {
						ps[i].LastError = msg
						if err == nil {
							ps[i].LastSync = time.Now()
						}
					}
				}
				return ps, nil
			})
		}(p)
	}
}

func (c *Manager) sync(ctx context.Context, p Protected) error {
	owner, err := c.Node(ctx, p.Owner)
	if err != nil {
		return err
	}
	replica, err := c.Node(ctx, p.Replica)
	if err != nil {
		return err
	}
	vms, err := replica.VMs(ctx)
	if err != nil {
		return err
	}
	exists := slices.ContainsFunc(vms, func(v kube.VM) bool { return v.Name == p.VM })
	if exists {
		if err := zsync(ctx, p.VM, owner, replica); !errors.Is(err, errNoZFS) {
			return err
		}
	}
	var ex kube.Export
	if err := owner.do(ctx, http.MethodPost, vmPath(p.VM, "snapshot-export"), map[string]string{"baseURL": owner.URL}, &ex); err != nil {
		return err
	}
	defer owner.do(context.Background(), http.MethodPost, vmPath(p.VM, "unexport"), nil, nil)
	if !exists {
		return c.firstReplica(ctx, p, replica, ex)
	}
	var mapping map[string]string
	if err := replica.do(ctx, http.MethodPost, vmPath(p.VM, "replica/refresh"), map[string]any{"export": ex}, &mapping); err != nil {
		return err
	}
	abort := func(cause error) error {
		cctx, cancel := cleanupCtx()
		defer cancel()
		replica.do(cctx, http.MethodPost, vmPath(p.VM, "replica/abort"), map[string]any{"mapping": mapping}, nil)
		return cause
	}
	failed, err := kube.WaitDisks(ctx, 5*time.Second, nodeDisks(replica, vmPath(p.VM, "replica/status"), map[string]any{"mapping": mapping}), nil)
	if failed != "" {
		err = fmt.Errorf("disk %s failed to copy; previous replica kept", failed)
	}
	if err != nil {
		return abort(err)
	}
	if err := replica.do(ctx, http.MethodPost, vmPath(p.VM, "replica/swap"), map[string]any{"export": ex, "mapping": mapping}, nil); err != nil {
		return abort(err)
	}
	return nil
}

func (c *Manager) firstReplica(ctx context.Context, p Protected, replica Node, ex kube.Export) error {
	if err := replica.do(ctx, http.MethodPost, "/api/import", map[string]any{"name": p.VM, "operation": kube.HAReplicaOperation, "export": ex}, nil); err != nil {
		return err
	}
	defer replica.do(context.Background(), http.MethodPost, vmPath(p.VM, "imported"), nil, nil)
	if err := replica.do(ctx, http.MethodPost, vmPath(p.VM, "ha"), map[string]bool{"enabled": true}, nil); err != nil {
		return err
	}
	failed, err := kube.WaitDisks(ctx, 5*time.Second, nodeDisks(replica, vmPath(p.VM, "disks"), nil), nil)
	if failed != "" {
		return fmt.Errorf("disk %s failed to copy", failed)
	}
	return err
}

func recovering(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: recovered from panic: %v", name, r)
		}
	}()
	fn()
}
