package manager

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/tlscert"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const nodesKey = "argus-nodes"

type Node struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Token      string `json:"token"`
	CA         string `json:"ca"`
	Slot       int    `json:"slot,omitempty"`
	Org        string `json:"org"`
	Datacenter string `json:"datacenter"`
	Cluster    string `json:"cluster"`
}

type NodeInfo = kube.NodeInfo

type Manager struct {
	store auth.Secrets

	mu       sync.Mutex
	jobs     []*Job
	lastSeen map[string]time.Time
	syncing  map[string]bool
	started  time.Time
	haMu     sync.Mutex
	inv      inventory
}

type Job struct {
	Org      string
	VM       string
	From, To string
	Started  time.Time
	Status   string
	Done     bool
	Failed   bool
	Live     bool
}

func New(store auth.Secrets) *Manager {
	return &Manager{store: store, lastSeen: map[string]time.Time{}, syncing: map[string]bool{}, started: time.Now(), inv: inventory{nodes: map[string]NodeState{}}}
}

func (c *Manager) Nodes(ctx context.Context) ([]Node, error) {
	ns, _, err := c.load(ctx)
	return ns, err
}

func (c *Manager) NodesFor(ctx context.Context, org string) ([]Node, error) {
	ns, err := c.Nodes(ctx)
	out := ns[:0]
	for _, n := range ns {
		if n.Org == org {
			out = append(out, n)
		}
	}
	return out, err
}

func (c *Manager) NodeIn(ctx context.Context, org, name string) (Node, error) {
	n, err := c.Node(ctx, name)
	if err == nil && n.Org != org {
		return Node{}, fmt.Errorf("node %s not found", name)
	}
	return n, err
}

func (c *Manager) SetNodeGroup(ctx context.Context, org, name, datacenter, cluster string) error {
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	for i := range ns {
		if ns[i].Name == name && ns[i].Org == org {
			ns[i].Datacenter, ns[i].Cluster = datacenter, cluster
			return c.save(ctx, ns, rv)
		}
	}
	return fmt.Errorf("node %s not found", name)
}

func (c *Manager) Node(ctx context.Context, name string) (Node, error) {
	ns, _, err := c.load(ctx)
	if err != nil {
		return Node{}, err
	}
	for _, n := range ns {
		if n.Name == name {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("node %s not found", name)
}

func (c *Manager) load(ctx context.Context) ([]Node, string, error) {
	data, rv, err := c.store.Get(ctx, nodesKey)
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var ns []Node
	return ns, rv, json.Unmarshal(data["nodes"], &ns)
}

func (c *Manager) save(ctx context.Context, ns []Node, rv string) error {
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	b, err := json.Marshal(ns)
	if err != nil {
		return err
	}
	data := map[string][]byte{"nodes": b}
	if rv == "" {
		return c.store.Create(ctx, nodesKey, "Opaque", data)
	}
	return c.store.Update(ctx, nodesKey, rv, data)
}

func (c *Manager) AddNode(ctx context.Context, org, name, url, token, fingerprint, datacenter, cluster string) error {
	url = strings.TrimRight(url, "/")
	ca, err := fetchCA(ctx, url)
	if err != nil {
		return err
	}
	if got := tlscert.Fingerprint(ca); !strings.EqualFold(got, strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", "")) {
		return fmt.Errorf("CA fingerprint mismatch: node presents %s", got)
	}
	n := Node{Name: name, URL: url, Token: token, CA: string(ca), Org: org, Datacenter: datacenter, Cluster: cluster}
	if _, err := n.Info(ctx); err != nil {
		return err
	}
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	for _, x := range ns {
		if x.Name == name || x.URL == url {
			return errors.New("node already registered")
		}
	}
	if err := c.save(ctx, append(ns, n), rv); err != nil {
		return err
	}
	return c.PushMigrationNetwork(ctx)
}

func (c *Manager) RemoveNode(ctx context.Context, org, name string) error {
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	out := ns[:0]
	for _, n := range ns {
		if n.Name != name || n.Org != org {
			out = append(out, n)
		}
	}
	return c.save(ctx, out, rv)
}

func fetchCA(ctx context.Context, url string) ([]byte, error) {
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/ca.pem", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch CA: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

func (n Node) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(n.CA))
	return &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

func (n Node) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s: %s", n.Name, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (n Node) Info(ctx context.Context) (NodeInfo, error) {
	var info NodeInfo
	return info, n.do(ctx, http.MethodGet, "/api/node", nil, &info)
}

func (n Node) VMs(ctx context.Context) ([]kube.VM, error) {
	var vms []kube.VM
	return vms, n.do(ctx, http.MethodGet, "/api/vms", nil, &vms)
}

func (n Node) Action(ctx context.Context, ns, vm, action string) error {
	return n.do(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/%s", ns, vm, action), nil, nil)
}

func (c *Manager) checkNetworks(ctx context.Context, src, dst Node, ns, vm string) error {
	vms, err := src.VMs(ctx)
	if err != nil {
		return err
	}
	var source *kube.VM
	for i := range vms {
		if vms[i].Namespace == ns && vms[i].Name == vm {
			source = &vms[i]
		}
	}
	if source == nil {
		return fmt.Errorf("%s not found on %s", vm, src.Name)
	}
	var nets []kube.Network
	if err := dst.do(ctx, http.MethodGet, "/api/networks", nil, &nets); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range nets {
		have[n.Name] = true
		have[kube.Namespace+"/"+n.Name] = true
	}
	for _, nic := range source.NICs {
		if nic.Network != "" && nic.Network != "pod" && !have[nic.Network] {
			return fmt.Errorf("network %s missing on %s", nic.Network, dst.Name)
		}
	}
	return nil
}

func (c *Manager) Jobs(org string) []Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Job, 0, len(c.jobs))
	for i := len(c.jobs) - 1; i >= 0; i-- {
		if c.jobs[i].Org == org {
			out = append(out, *c.jobs[i])
		}
	}
	return out
}

func (c *Manager) update(j *Job, status string, done, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j.Status, j.Done, j.Failed = status, done, failed
}

func (c *Manager) Migrate(src, dst Node, ns, vm string) {
	j := &Job{Org: src.Org, VM: vm, From: src.Name, To: dst.Name, Started: time.Now(), Status: "Starting"}
	c.mu.Lock()
	c.jobs = append(c.jobs, j)
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		if err := c.migrate(ctx, j, src, dst, ns, vm); err != nil {
			c.update(j, "Failed: "+err.Error(), true, true)
			return
		}
		c.update(j, "Completed", true, false)
	}()
}

func (c *Manager) migrate(ctx context.Context, j *Job, src, dst Node, ns, vm string) error {
	if err := c.checkNetworks(ctx, src, dst, ns, vm); err != nil {
		return err
	}

	c.update(j, "Exporting", false, false)
	var ex kube.Export
	if err := src.do(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/export", ns, vm), map[string]string{"baseURL": src.URL}, &ex); err != nil {
		return err
	}
	unexport := func() {
		src.do(context.Background(), http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/unexport", ns, vm), nil, nil)
	}

	c.update(j, "Importing", false, false)
	if err := dst.do(ctx, http.MethodPost, "/api/import", map[string]any{"name": vm, "export": ex}, nil); err != nil {
		unexport()
		return err
	}
	rollback := func() {
		dst.do(context.Background(), http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/delete", kube.Namespace, vm), nil, nil)
		dst.do(context.Background(), http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/imported", kube.Namespace, vm), nil, nil)
		unexport()
	}
	for {
		var disks []kube.DiskStatus
		if err := dst.do(ctx, http.MethodGet, fmt.Sprintf("/api/vms/%s/%s/disks", kube.Namespace, vm), nil, &disks); err != nil {
			rollback()
			return err
		}
		done, progress := true, []string{}
		for _, d := range disks {
			if d.Phase == "Failed" {
				rollback()
				return fmt.Errorf("disk %s failed to copy", d.Name)
			}
			if d.Phase != "Succeeded" {
				done = false
			}
			progress = append(progress, d.Name+" "+d.Progress)
		}
		if done {
			break
		}
		c.update(j, "Copying "+strings.Join(progress, ", "), false, false)
		select {
		case <-ctx.Done():
			rollback()
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	c.update(j, "Cleaning up", false, false)
	dst.do(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/imported", kube.Namespace, vm), nil, nil)
	unexport()
	return src.do(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/delete", ns, vm), nil, nil)
}

func (n Node) Call(ctx context.Context, method, path string, in, out any) error {
	return n.do(ctx, method, path, in, out)
}

func (n Node) DialConsole(ctx context.Context, ns, vm, kind string) (*websocket.Conn, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(n.CA))
	d := websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: pool}, Subprotocols: []string{"binary"}}
	u := strings.Replace(n.URL, "https://", "wss://", 1) + fmt.Sprintf("/api/vms/%s/%s/ws/%s", ns, vm, kind)
	conn, resp, err := d.DialContext(ctx, u, http.Header{"Authorization": {"Bearer " + n.Token}})
	if err != nil && resp != nil {
		return nil, fmt.Errorf("%s: console: %s", n.Name, resp.Status)
	}
	return conn, err
}
