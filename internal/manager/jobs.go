package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	"argus/internal/kube"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	jobsKey  = "argus-jobs"
	keepJobs = 200
)

var errUnknown = errors.New("outcome unknown; source and target left in place, check both hosts")

type Job struct {
	ID        string
	Kind      string
	Org       string
	Namespace string
	VM        string
	From, To  string
	Started   time.Time
	Status    string
	Done      bool
	Failed    bool
	Live      bool
	Attention bool
}

func busyKey(node, vm string) string { return node + "/" + vm }

func (c *Manager) Busy(node, vm string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy[busyKey(node, vm)]
}

func (c *Manager) startJob(j *Job, timeout time.Duration, run func(context.Context, *Job) error) error {
	keys := []string{busyKey(j.From, j.VM), busyKey(j.To, j.VM)}
	c.mu.Lock()
	if c.syncing[j.VM] {
		c.mu.Unlock()
		return fmt.Errorf("%s is being replicated; retry shortly", j.VM)
	}
	for _, k := range keys {
		if c.busy[k] {
			c.mu.Unlock()
			return fmt.Errorf("%s already has an operation in progress", j.VM)
		}
	}
	for _, k := range keys {
		c.busy[k] = true
	}
	id := make([]byte, 8)
	rand.Read(id)
	j.ID, j.Started, j.Status = hex.EncodeToString(id), time.Now(), "Starting"
	c.jobs = append(c.jobs, j)
	if len(c.jobs) > keepJobs {
		c.jobs = c.jobs[len(c.jobs)-keepJobs:]
	}
	c.mu.Unlock()
	if err := c.saveJobs(); err != nil {
		c.mu.Lock()
		c.jobs = slices.DeleteFunc(c.jobs, func(x *Job) bool { return x == j })
		for _, k := range keys {
			delete(c.busy, k)
		}
		c.mu.Unlock()
		return fmt.Errorf("cannot record the operation: %w", err)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := run(ctx, j); err != nil {
			c.mu.Lock()
			j.Attention = errors.Is(err, errUnknown)
			c.mu.Unlock()
			c.update(j, "Failed: "+err.Error(), true, true)
		} else {
			c.update(j, "Completed", true, false)
		}
		c.mu.Lock()
		if !j.Attention {
			for _, k := range keys {
				delete(c.busy, k)
			}
		}
		c.mu.Unlock()
	}()
	return nil
}

func (c *Manager) update(j *Job, status string, done, failed bool) {
	c.mu.Lock()
	j.Status, j.Done, j.Failed = status, done, failed
	c.mu.Unlock()
	c.saveJobs()
}

func (c *Manager) saveJobs() error {
	c.jobsMu.Lock()
	defer c.jobsMu.Unlock()
	c.mu.Lock()
	b, err := json.Marshal(c.jobs)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := map[string][]byte{"jobs": b}
	_, rv, err := c.store.Get(ctx, jobsKey)
	switch {
	case apierrors.IsNotFound(err):
		err = c.store.Create(ctx, jobsKey, "Opaque", data)
	case err == nil:
		err = c.store.Update(ctx, jobsKey, rv, data)
	}
	if err != nil {
		log.Printf("jobs: save: %v", err)
	}
	return err
}

func (c *Manager) loadJobs(ctx context.Context) {
	data, _, err := c.store.Get(ctx, jobsKey)
	if err != nil {
		return
	}
	var jobs []*Job
	if err := json.Unmarshal(data["jobs"], &jobs); err != nil {
		log.Printf("jobs: load: %v", err)
		return
	}
	for _, j := range jobs {
		if !j.Done && j.Kind != "" {
			j.Done, j.Failed = true, true
			j.Status = fmt.Sprintf("Interrupted by Manager restart during %q; check %s", j.Status, j.From)
			continue
		}
		if j.Attention || !j.Done {
			c.busy[busyKey(j.From, j.VM)], c.busy[busyKey(j.To, j.VM)] = true, true
		}
		if !j.Done {
			j.Done, j.Failed, j.Attention = true, true, true
			j.Status = fmt.Sprintf("Interrupted by Manager restart during %q; check %s and %s (operation %s)", j.Status, j.From, j.To, j.ID)
			log.Printf("jobs: %s", j.Status)
		}
	}
	c.mu.Lock()
	c.jobs = jobs
	c.mu.Unlock()
	c.saveJobs()
}

func (c *Manager) Resolve(ctx context.Context, org, id string) error {
	c.mu.Lock()
	var j *Job
	for _, x := range c.jobs {
		if x.ID == id && x.Org == org && x.Attention {
			j = x
		}
	}
	c.mu.Unlock()
	if j == nil {
		return errors.New("no unresolved job with that ID")
	}
	src, err := c.Node(ctx, j.From)
	if err != nil {
		return err
	}
	dst, err := c.Node(ctx, j.To)
	if err != nil {
		return err
	}
	c.mu.Lock()
	other := slices.ContainsFunc(c.jobs, func(x *Job) bool {
		return x != j && !x.Done && x.VM == j.VM && (x.From == j.From || x.To == j.To || x.From == j.To || x.To == j.From)
	})
	c.mu.Unlock()
	if other {
		return fmt.Errorf("%s has an operation in progress", j.VM)
	}
	ns := j.Namespace
	if ns == "" {
		ns = kube.Namespace
	}
	source, err := vmState(ctx, src, ns, j.VM)
	if err != nil {
		return fmt.Errorf("cannot confirm the source: %v", err)
	}
	if j.Live && !source.Running {
		return fmt.Errorf("%s is not running on %s; check both hosts manually", j.VM, j.From)
	}
	if target, err := vmState(ctx, dst, kube.Namespace, j.VM); err == nil && target.Running {
		return fmt.Errorf("%s is running on %s; check both hosts manually", j.VM, j.To)
	}
	op := map[string]string{"operation": j.ID}
	src.do(ctx, http.MethodPost, vmPathIn(ns, j.VM, "migration-cleanup"), op, nil)
	dst.do(ctx, http.MethodPost, vmPathIn(kube.Namespace, j.VM, "migration-cleanup"), op, nil)
	if err := dst.do(ctx, http.MethodPost, vmPathIn(kube.Namespace, j.VM, "abort"), op, nil); err != nil {
		return err
	}
	if !j.Live {
		dst.do(ctx, http.MethodPost, vmPathIn(kube.Namespace, j.VM, "imported"), nil, nil)
		src.do(ctx, http.MethodPost, vmPathIn(ns, j.VM, "unexport"), nil, nil)
	}
	c.mu.Lock()
	j.Attention = false
	delete(c.busy, busyKey(j.From, j.VM))
	delete(c.busy, busyKey(j.To, j.VM))
	c.mu.Unlock()
	c.update(j, "Rolled back: "+j.VM+" kept on "+j.From, true, true)
	return nil
}
