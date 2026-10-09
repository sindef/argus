package center

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"argus/internal/kube"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func (c *Center) Protected(ctx context.Context) ([]Protected, error) {
	ps, _, err := c.loadHA(ctx)
	return ps, err
}

func (c *Center) loadHA(ctx context.Context) ([]Protected, string, error) {
	data, rv, err := c.store.Get(ctx, haKey)
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var ps []Protected
	return ps, rv, json.Unmarshal(data["vms"], &ps)
}

func (c *Center) updateHA(ctx context.Context, fn func([]Protected) ([]Protected, error)) error {
	c.haMu.Lock()
	defer c.haMu.Unlock()
	ps, rv, err := c.loadHA(ctx)
	if err != nil {
		return err
	}
	if ps, err = fn(ps); err != nil {
		return err
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	data := map[string][]byte{"vms": b}
	if rv == "" {
		return c.store.Create(ctx, haKey, "Opaque", data)
	}
	return c.store.Update(ctx, haKey, rv, data)
}

func vmPath(vm, suffix string) string {
	return fmt.Sprintf("/api/vms/%s/%s/%s", kube.Namespace, vm, suffix)
}

func (c *Center) Protect(ctx context.Context, owner, vm, replica string, interval int) error {
	if owner == replica {
		return errors.New("replica must be on another node")
	}
	o, err := c.Node(ctx, owner)
	if err != nil {
		return err
	}
	if _, err := c.Node(ctx, replica); err != nil {
		return err
	}
	if interval < 5 {
		return errors.New("interval must be at least 5 minutes")
	}
	err = c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		for _, p := range ps {
			if p.VM == vm {
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

func (c *Center) Unprotect(ctx context.Context, vm string) error {
	var p Protected
	err := c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		out := ps[:0]
		for _, x := range ps {
			if x.VM == vm {
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
		r.do(ctx, http.MethodPost, vmPath(vm, "delete"), nil, nil)
	}
	return nil
}

func (c *Center) Start() {
	c.started = time.Now()
	go func() {
		for range time.Tick(heartbeatEvery) {
			ctx, cancel := context.WithTimeout(context.Background(), heartbeatEvery)
			c.heartbeat(ctx)
			c.failover(ctx)
			cancel()
		}
	}()
	go func() {
		for range time.Tick(30 * time.Second) {
			c.replicate()
		}
	}()
}

func (c *Center) LastSeen(node string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen[node]
}

func (c *Center) heartbeat(ctx context.Context) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return
	}
	ps, err := c.Protected(ctx)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, n := range nodes {
		owned := []string{}
		for _, p := range ps {
			if p.Owner == n.Name {
				owned = append(owned, p.VM)
			}
		}
		peers := []string{}
		for _, o := range nodes {
			if o.Name != n.Name {
				peers = append(peers, o.URL)
			}
		}
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			if err := n.do(ctx, http.MethodPost, "/api/heartbeat", map[string]any{"owned": owned, "peers": peers}, nil); err == nil {
				c.mu.Lock()
				c.lastSeen[n.Name] = time.Now()
				c.mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
}

func (c *Center) down(node string) bool {
	seen := c.LastSeen(node)
	if seen.IsZero() {
		seen = c.started
	}
	return time.Since(seen) > failoverAfter
}

func (c *Center) failover(ctx context.Context) {
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
		if err1 != nil || err2 != nil {
			continue
		}
		var probe struct{ Reachable bool }
		if err := replica.do(ctx, http.MethodGet, "/api/probe?url="+url.QueryEscape(owner.URL), nil, &probe); err != nil || probe.Reachable {
			continue
		}
		vm := p.VM
		err := c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
			for i := range ps {
				if ps[i].VM == vm {
					ps[i].Owner, ps[i].Replica = ps[i].Replica, ps[i].Owner
					ps[i].LastSync = time.Time{}
				}
			}
			return ps, nil
		})
		if err != nil {
			continue
		}
		log.Printf("ha: %s failed over from %s to %s", vm, owner.Name, replica.Name)
		c.mu.Lock()
		c.jobs = append(c.jobs, &Job{VM: vm, From: owner.Name, To: replica.Name, Started: time.Now(), Status: "Failed over (HA)", Done: true})
		c.mu.Unlock()
	}
}

func (c *Center) replicate() {
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
		busy := c.syncing[p.VM]
		c.syncing[p.VM] = true
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
			err := c.sync(sctx, p)
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

func (c *Center) sync(ctx context.Context, p Protected) error {
	owner, err := c.Node(ctx, p.Owner)
	if err != nil {
		return err
	}
	replica, err := c.Node(ctx, p.Replica)
	if err != nil {
		return err
	}
	var ex kube.Export
	if err := owner.do(ctx, http.MethodPost, vmPath(p.VM, "snapshot-export"), map[string]string{"baseURL": owner.URL}, &ex); err != nil {
		return err
	}
	defer owner.do(context.Background(), http.MethodPost, vmPath(p.VM, "unexport"), nil, nil)

	err = c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		for i := range ps {
			if ps[i].VM == p.VM {
				ps[i].LastSync = time.Time{}
			}
		}
		return ps, nil
	})
	if err != nil {
		return err
	}
	replica.do(ctx, http.MethodPost, vmPath(p.VM, "delete"), nil, nil)
	for {
		vms, err := replica.VMs(ctx)
		if err != nil {
			return err
		}
		present := false
		for _, v := range vms {
			present = present || v.Name == p.VM
		}
		if !present {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if err := replica.do(ctx, http.MethodPost, "/api/import", map[string]any{"name": p.VM, "export": ex}, nil); err != nil {
		return err
	}
	defer replica.do(context.Background(), http.MethodPost, vmPath(p.VM, "imported"), nil, nil)
	if err := replica.do(ctx, http.MethodPost, vmPath(p.VM, "ha"), map[string]bool{"enabled": true}, nil); err != nil {
		return err
	}
	for {
		var disks []kube.DiskStatus
		if err := replica.do(ctx, http.MethodGet, vmPath(p.VM, "disks"), nil, &disks); err != nil {
			return err
		}
		done := true
		for _, d := range disks {
			if d.Phase == "Failed" {
				return fmt.Errorf("disk %s failed to copy", d.Name)
			}
			done = done && d.Phase == "Succeeded"
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
