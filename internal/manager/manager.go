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
	"log"
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
	"k8s.io/apimachinery/pkg/api/resource"
)

const nodesKey = "argus-nodes"

type Node struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	Token            string `json:"token"`
	CA               string `json:"ca"`
	Slot             int    `json:"slot,omitempty"`
	MigrationAddress string `json:"migrationAddress,omitempty"`
	Org              string `json:"org"`
	Datacenter       string `json:"datacenter"`
	Cluster          string `json:"cluster"`
}

type NodeInfo = kube.NodeInfo

type Manager struct {
	store auth.Secrets

	mu       sync.Mutex
	jobsMu   sync.Mutex
	jobs     []*Job
	busy     map[string]bool
	lastSeen map[string]time.Time
	syncing  map[string]bool
	started  time.Time
	haMu     sync.Mutex
	inv      inventory
	tel      *telemetry
}

func New(store auth.Secrets, metricsPath string) *Manager {
	return &Manager{store: store, lastSeen: map[string]time.Time{}, busy: map[string]bool{}, syncing: map[string]bool{}, started: time.Now(), inv: inventory{nodes: map[string]NodeState{}}, tel: newTelemetry(metricsPath)}
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
	var removed *Node
	out := make([]Node, 0, len(ns))
	for i, n := range ns {
		if n.Name == name && n.Org == org {
			removed = &ns[i]
			continue
		}
		out = append(out, n)
	}
	if removed == nil {
		return fmt.Errorf("host %s not found", name)
	}
	c.releaseSlot(ctx, *removed)
	if err := c.save(ctx, out, rv); err != nil {
		return err
	}
	dropClients(name)
	return nil
}

func fetchCA(ctx context.Context, url string) ([]byte, error) {
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
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

var clients = struct {
	sync.Mutex
	m map[string]*http.Client
}{m: map[string]*http.Client{}}

func (n Node) client() *http.Client {
	key := n.Name + "\x00" + n.URL + "\x00" + n.CA
	clients.Lock()
	defer clients.Unlock()
	if c, ok := clients.m[key]; ok {
		return c
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(n.CA))
	c := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	clients.m[key] = c
	return c
}

func dropClients(name string) {
	clients.Lock()
	defer clients.Unlock()
	for k, c := range clients.m {
		if strings.HasPrefix(k, name+"\x00") {
			c.CloseIdleConnections()
			delete(clients.m, k)
		}
	}
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

func (c *Manager) preflight(ctx context.Context, src, dst Node, ns, vm string) error {
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
	var srcNets, dstNets []kube.Network
	if err := src.do(ctx, http.MethodGet, "/api/networks", nil, &srcNets); err != nil {
		return err
	}
	if err := dst.do(ctx, http.MethodGet, "/api/networks", nil, &dstNets); err != nil {
		return err
	}
	have := map[string]kube.Network{}
	for _, n := range dstNets {
		have[n.Name] = n
	}
	for _, sn := range srcNets {
		used := false
		for _, nic := range source.NICs {
			used = used || nic.Network == sn.Name || nic.Network == kube.Namespace+"/"+sn.Name
		}
		if !used {
			continue
		}
		dn, ok := have[sn.Name]
		switch {
		case !ok:
			return fmt.Errorf("network %s missing on %s", sn.Name, dst.Name)
		case dn.VLAN != sn.VLAN:
			return fmt.Errorf("network %s is VLAN %d on %s but VLAN %d on %s", sn.Name, sn.VLAN, src.Name, dn.VLAN, dst.Name)
		}
	}
	var st struct{ Pools []kube.Pool }
	if err := dst.do(ctx, http.MethodGet, "/api/storage", nil, &st); err != nil {
		return err
	}
	var free int64
	for _, p := range st.Pools {
		free += p.Free
	}
	if free < source.StorageBytes {
		return fmt.Errorf("%s has %d GiB free, %s needs %d GiB", dst.Name, free>>30, vm, source.StorageBytes>>30)
	}
	if source.Running {
		var m kube.MetricsSample
		if err := dst.do(ctx, http.MethodGet, "/api/metrics", nil, &m); err != nil {
			return err
		}
		need, _ := resource.ParseQuantity(source.Memory)
		if avail := m.MemTotal - m.MemUsed; avail < need.Value() {
			return fmt.Errorf("%s has %d MiB memory available, %s needs %d MiB", dst.Name, avail>>20, vm, need.Value()>>20)
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

func (c *Manager) Migrate(src, dst Node, ns, vm string) error {
	return c.startJob(&Job{Org: src.Org, Namespace: ns, VM: vm, From: src.Name, To: dst.Name}, 12*time.Hour, func(ctx context.Context, j *Job) error {
		return c.migrate(ctx, j, src, dst, ns, vm)
	})
}

func vmPathIn(ns, vm, action string) string {
	if action == "" {
		return fmt.Sprintf("/api/vms/%s/%s", ns, vm)
	}
	return fmt.Sprintf("/api/vms/%s/%s/%s", ns, vm, action)
}

func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func (c *Manager) migrate(ctx context.Context, j *Job, src, dst Node, ns, vm string) error {
	if err := c.preflight(ctx, src, dst, ns, vm); err != nil {
		return err
	}
	op := map[string]string{"operation": j.ID}

	c.update(j, "Exporting", false, false)
	var ex kube.Export
	if err := src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "export"), map[string]string{"baseURL": src.URL}, &ex); err != nil {
		return err
	}
	unexport := func() {
		cctx, cancel := cleanupCtx()
		defer cancel()
		if err := src.do(cctx, http.MethodPost, vmPathIn(ns, vm, "unexport"), nil, nil); err != nil {
			log.Printf("migrate %s: unexport: %v", vm, err)
		}
	}

	c.update(j, "Importing", false, false)
	if err := dst.do(ctx, http.MethodPost, "/api/import", map[string]any{"name": vm, "operation": j.ID, "export": ex}, nil); err != nil {
		unexport()
		return err
	}
	rollback := func(cause error) error {
		cctx, cancel := cleanupCtx()
		defer cancel()
		err := dst.do(cctx, http.MethodPost, vmPathIn(kube.Namespace, vm, "abort"), op, nil)
		dst.do(cctx, http.MethodPost, vmPathIn(kube.Namespace, vm, "imported"), nil, nil)
		unexport()
		if err != nil {
			return fmt.Errorf("%w; target cleanup failed: %v", cause, err)
		}
		return cause
	}
	for {
		var disks []kube.DiskStatus
		if err := dst.do(ctx, http.MethodGet, vmPathIn(kube.Namespace, vm, "disks"), nil, &disks); err != nil {
			if ctx.Err() != nil {
				return rollback(ctx.Err())
			}
			c.update(j, "Copying (target unreachable, retrying)", false, false)
		} else {
			done, progress := true, []string{}
			for _, d := range disks {
				if d.Phase == "Failed" {
					return rollback(fmt.Errorf("disk %s failed to copy", d.Name))
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
		}
		select {
		case <-ctx.Done():
			return rollback(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}

	c.update(j, "Cleaning up", false, false)
	dst.do(ctx, http.MethodPost, vmPathIn(kube.Namespace, vm, "imported"), nil, nil)
	unexport()
	return src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "delete"), nil, nil)
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
