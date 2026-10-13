package manager

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/wire"

	"k8s.io/apimachinery/pkg/api/resource"
)

func (c *Manager) setNode(ctx context.Context, name string, fn func(*Node)) (Node, error) {
	ns, rv, err := c.load(ctx)
	if err != nil {
		return Node{}, err
	}
	for i := range ns {
		if ns[i].Name == name {
			fn(&ns[i])
			return ns[i], c.save(ctx, ns, rv)
		}
	}
	return Node{}, fmt.Errorf("node %s not found", name)
}

func (c *Manager) startTask(ctx context.Context, j *Job, lock string, timeout time.Duration, run func(context.Context, *Job) error) error {
	key := "task:" + lock
	c.mu.Lock()
	if c.busy[key] {
		c.mu.Unlock()
		return fmt.Errorf("%s already has a task running", lock)
	}
	c.busy[key] = true
	id := make([]byte, 8)
	rand.Read(id)
	j.ID, j.Started, j.Status, j.User = hex.EncodeToString(id), time.Now(), "Starting", cmp.Or(j.User, auth.Actor(ctx))
	c.jobs = append(c.jobs, j)
	if len(c.jobs) > keepJobs {
		c.jobs = c.jobs[len(c.jobs)-keepJobs:]
	}
	c.mu.Unlock()
	if err := c.saveJobs(); err != nil {
		c.mu.Lock()
		c.jobs = slices.DeleteFunc(c.jobs, func(x *Job) bool { return x == j })
		delete(c.busy, key)
		c.mu.Unlock()
		return fmt.Errorf("cannot record the task: %w", err)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := run(ctx, j); err != nil {
			log.Printf("task %s %s: %v", j.Kind, j.From, err)
			c.finish(j, err)
		} else {
			c.finish(j, nil)
		}
		c.mu.Lock()
		delete(c.busy, key)
		c.mu.Unlock()
	}()
	return nil
}

func (c *Manager) waitJob(ctx context.Context, j *Job) error {
	for {
		c.mu.Lock()
		done, failed, status := j.Done, j.Failed, j.Status
		c.mu.Unlock()
		if done {
			if failed {
				return errors.New(strings.TrimPrefix(status, "Failed: "))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Manager) EnterMaintenance(ctx context.Context, org, host string, shutdown bool) error {
	n, err := c.NodeIn(context.Background(), org, host)
	if err != nil {
		return err
	}
	return c.startTask(ctx, &Job{Org: org, Kind: "Enter maintenance", From: host}, host, 12*time.Hour, func(ctx context.Context, j *Job) error {
		return c.evacuate(ctx, j, n, shutdown)
	})
}

func (c *Manager) ExitMaintenance(ctx context.Context, org, host string) error {
	n, err := c.NodeIn(context.Background(), org, host)
	if err != nil {
		return err
	}
	return c.startTask(ctx, &Job{Org: org, Kind: "Exit maintenance", From: host}, host, time.Hour, func(ctx context.Context, j *Job) error {
		return c.leave(ctx, j, n)
	})
}

func memBytes(q string) int64 {
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return 0
	}
	return v.Value()
}

func (c *Manager) arch(node string) string {
	st, _ := c.NodeState(node)
	return st.Arch()
}

func (c *Manager) targets(ctx context.Context, src Node) []Node {
	nodes, _ := c.NodesFor(ctx, src.Org)
	srcState, _ := c.NodeState(src.Name)
	type cand struct {
		n    Node
		free int64
		same bool
	}
	var cs []cand
	for _, n := range nodes {
		if n.Name == src.Name || n.Maintenance || n.Datacenter != src.Datacenter || n.Cluster != src.Cluster {
			continue
		}
		st, ok := c.NodeState(n.Name)
		if !ok || !st.Reachable() {
			continue
		}
		free := st.Info.MemoryBytes
		for _, v := range st.VMs {
			if v.Running {
				free -= memBytes(v.Memory)
			}
		}
		cs = append(cs, cand{n, free, st.Arch() == srcState.Arch()})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].same != cs[j].same {
			return cs[i].same
		}
		return cs[i].free > cs[j].free
	})
	out := make([]Node, len(cs))
	for i, x := range cs {
		out[i] = x.n
	}
	return out
}

func (c *Manager) evacuate(ctx context.Context, j *Job, n Node, shutdown bool) error {
	n, err := c.setNode(ctx, n.Name, func(x *Node) { x.Maintenance = true })
	if err != nil {
		return err
	}
	vms, err := n.VMs(ctx)
	if err != nil {
		return err
	}
	ps, err := c.Protected(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		if !v.Running {
			continue
		}
		label := v.Namespace + "/" + v.Name
		if i := slices.IndexFunc(ps, func(p Protected) bool { return p.VM == v.Name && p.Owner == n.Name }); i >= 0 {
			c.progress(j, "Failing over "+v.Name+" to "+ps[i].Replica)
			if err := c.plannedFailover(ctx, n, ps[i]); err != nil {
				return fmt.Errorf("%s: %w", v.Name, err)
			}
			continue
		}
		moved := false
		for _, dst := range c.targets(ctx, n) {
			if c.preflight(ctx, n, dst, v.Namespace, v.Name) != nil {
				continue
			}
			if _, err := c.carry(ctx, n, dst, v.Namespace, v.Name); err != nil {
				continue
			}
			status := "Migrating " + v.Name + " to " + dst.Name
			if a, b := c.arch(n.Name), c.arch(dst.Name); a != b {
				status += " (warning: " + b + " host, VM came from " + a + ")"
			}
			c.progress(j, status)
			mj := &Job{Org: n.Org, Namespace: v.Namespace, VM: v.Name, From: n.Name, To: dst.Name, Live: true, User: j.User}
			if err := c.startJob(ctx, mj, 6*time.Hour, func(ctx context.Context, mj *Job) error {
				return c.liveMigrate(ctx, mj, n, dst, v.Namespace, v.Name)
			}); err != nil {
				return fmt.Errorf("%s: %w", v.Name, err)
			}
			if err := c.waitJob(ctx, mj); err != nil {
				return fmt.Errorf("%s: %w", v.Name, err)
			}
			moved = true
			break
		}
		if moved {
			continue
		}
		if !shutdown {
			return fmt.Errorf("no host in the cluster can take %s; allow shutting down VMs that cannot move", v.Name)
		}
		c.progress(j, "Shutting down "+v.Name)
		if err := n.Action(ctx, v.Namespace, v.Name, "stop"); err != nil {
			return fmt.Errorf("%s: %w", v.Name, err)
		}
		if _, err := c.setNode(ctx, n.Name, func(x *Node) {
			if !slices.Contains(x.Parked, label) {
				x.Parked = append(x.Parked, label)
			}
		}); err != nil {
			return err
		}
	}
	c.progress(j, "Waiting for VMs to stop")
	return waitFor(ctx, 10*time.Minute, func() (bool, error) {
		var st wire.HostStatus
		return n.do(ctx, http.MethodGet, "/api/host/status", nil, &st) == nil && st.Running == 0, nil
	})
}

func waitFor(ctx context.Context, limit time.Duration, ok func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		done, err := ok()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("timed out")
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Manager) plannedFailover(ctx context.Context, owner Node, p Protected) error {
	c.mu.Lock()
	if c.syncing[p.VM] {
		c.mu.Unlock()
		return errors.New("replication in progress; retry shortly")
	}
	c.syncing[p.VM] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.syncing, p.VM)
		c.mu.Unlock()
	}()
	if err := owner.Action(ctx, kube.Namespace, p.VM, "stop"); err != nil {
		return err
	}
	if err := waitFor(ctx, 10*time.Minute, func() (bool, error) {
		v, err := vmState(ctx, owner, kube.Namespace, p.VM)
		return err == nil && v.Status == "Stopped", nil
	}); err != nil {
		return fmt.Errorf("did not stop: %w", err)
	}
	if err := c.sync(ctx, p); err != nil {
		if serr := owner.Action(ctx, kube.Namespace, p.VM, "start"); serr != nil {
			return fmt.Errorf("final sync failed and restarting on %s failed: %w", owner.Name, errors.Join(err, serr))
		}
		return fmt.Errorf("final sync failed, left on %s: %w", owner.Name, err)
	}
	if err := c.updateHA(ctx, func(ps []Protected) ([]Protected, error) {
		for i := range ps {
			if ps[i].VM == p.VM && ps[i].Owner == p.Owner {
				ps[i].Owner, ps[i].Replica, ps[i].LastSync = p.Replica, p.Owner, time.Time{}
			}
		}
		return ps, nil
	}); err != nil {
		return err
	}
	replica, err := c.Node(ctx, p.Replica)
	if err != nil {
		return err
	}
	if err := replica.do(ctx, http.MethodPost, vmPath(p.VM, "replica/claim"), wire.ReplicaRequest{}, nil); err != nil {
		log.Printf("ha: clear the stop flag of %s on %s: %v", p.VM, replica.Name, err)
	}
	return waitFor(ctx, 10*time.Minute, func() (bool, error) {
		v, err := vmState(ctx, replica, kube.Namespace, p.VM)
		return err == nil && v.Running, nil
	})
}

func (c *Manager) leave(ctx context.Context, j *Job, n Node) error {
	n, err := c.setNode(ctx, n.Name, func(x *Node) { x.Maintenance = false })
	if err != nil {
		return err
	}
	for _, label := range n.Parked {
		ns, vm, _ := strings.Cut(label, "/")
		c.progress(j, "Starting "+vm)
		if err := waitFor(ctx, 10*time.Minute, func() (bool, error) {
			return n.Action(ctx, ns, vm, "start") == nil, nil
		}); err != nil {
			return fmt.Errorf("could not start %s: %w", vm, err)
		}
		if _, err := c.setNode(ctx, n.Name, func(x *Node) {
			x.Parked = slices.DeleteFunc(x.Parked, func(s string) bool { return s == label })
		}); err != nil {
			return err
		}
	}
	c.Refresh(n.Name)
	return nil
}

func (c *Manager) RollingUpdate(ctx context.Context, org, dc, cluster, image string, shutdown bool) error {
	nodes, err := c.NodesFor(context.Background(), org)
	if err != nil {
		return err
	}
	var members []Node
	for _, n := range nodes {
		if n.Datacenter == dc && n.Cluster == cluster {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return errors.New("no hosts in this cluster")
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	kind := "Rolling reboot"
	if image != "" {
		kind = "Rolling update to " + image
	}
	return c.startTask(ctx, &Job{Org: org, Kind: kind, From: groupLabel(dc, cluster)}, "cluster:"+dc+"/"+cluster, 24*time.Hour, func(ctx context.Context, j *Job) error {
		for i, n := range members {
			prefix := fmt.Sprintf("%s (%d/%d): ", n.Name, i+1, len(members))
			step := func(s string) { c.progress(j, prefix+s) }
			if err := c.cycleHost(ctx, n, image, shutdown, step); err != nil {
				return fmt.Errorf("%s%w; %s left in maintenance", prefix, err, n.Name)
			}
		}
		return nil
	})
}

func groupLabel(dc, cluster string) string {
	if dc == "" {
		dc = "Unassigned"
	}
	if cluster == "" {
		cluster = "Unassigned"
	}
	return dc + " / " + cluster
}

func (c *Manager) cycleHost(ctx context.Context, n Node, image string, shutdown bool, step func(string)) error {
	sub := &Job{}
	step("entering maintenance")
	if err := c.evacuate(ctx, sub, n, shutdown); err != nil {
		return err
	}
	var before wire.HostStatus
	if err := n.do(ctx, http.MethodGet, "/api/host/status", nil, &before); err != nil {
		return err
	}
	action, body := "reboot", map[string]string{}
	if image != "" {
		action, body["image"] = "upgrade", image
	}
	step(action + " requested")
	if err := n.do(ctx, http.MethodPost, "/api/host/power/"+action, body, nil); err != nil {
		return err
	}
	if err := waitFor(ctx, 30*time.Minute, func() (bool, error) {
		var st wire.HostStatus
		if reachable := n.do(ctx, http.MethodGet, "/api/host/status", nil, &st) == nil; !reachable {
			return true, nil
		}
		if st.State == wire.HostFailed || st.State == wire.HostCancelled {
			return false, errors.New(st.Status)
		}
		step(strings.ToLower(st.Status))
		return false, nil
	}); err != nil {
		return err
	}
	step("waiting for the host to return")
	var after wire.HostStatus
	if err := waitFor(ctx, 30*time.Minute, func() (bool, error) {
		if reachable := n.do(ctx, http.MethodGet, "/api/host/status", nil, &after) == nil; !reachable {
			return false, nil
		}
		if !after.Ready {
			step("back, waiting for " + after.Reason)
		}
		return after.Ready, nil
	}); err != nil {
		return fmt.Errorf("did not become healthy (%s): %w", after.Reason, err)
	}
	if image != "" && after.Version == before.Version {
		return fmt.Errorf("came back on %s; the update did not apply", after.Version)
	}
	step("exiting maintenance")
	return c.leave(ctx, sub, n)
}

func (c *Manager) LeastLoaded(org string) (Node, error) {
	var best Node
	var bestFree int64 = -1
	for _, st := range c.Inventory(org) {
		if !st.Reachable() || st.Node.Maintenance {
			continue
		}
		free := st.Info.MemoryBytes
		for _, v := range st.VMs {
			if v.Running {
				free -= memBytes(v.Memory)
			}
		}
		if free > bestFree {
			best, bestFree = st.Node, free
		}
	}
	if bestFree < 0 {
		return Node{}, errors.New("no healthy host available")
	}
	return best, nil
}
