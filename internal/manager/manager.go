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
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/store"
	"argus/internal/tlscert"
	"argus/internal/wire"

	"github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/api/resource"
)

const nodesKey = "argus-nodes"

type Node struct {
	Name             string   `json:"name"`
	URL              string   `json:"url"`
	Token            string   `json:"token"`
	CA               string   `json:"ca"`
	Slot             int      `json:"slot,omitempty"`
	MigrationAddress string   `json:"migrationAddress,omitempty"`
	Org              string   `json:"org"`
	Datacenter       string   `json:"datacenter"`
	Cluster          string   `json:"cluster"`
	Maintenance      bool     `json:"maintenance,omitempty"`
	Parked           []string `json:"parked,omitempty"`
}

type NodeInfo = kube.NodeInfo

type Manager struct {
	store        store.Secrets
	storeMu      sync.Mutex
	replications chan struct{}
	started      time.Time
	st           state
	saveMu       sync.Mutex
	inv          inventory
	tel          *telemetry
}

type state struct {
	mu       sync.Mutex
	jobs     []*Job
	busy     map[string]bool
	lastSeen map[string]time.Time
	syncing  map[string]bool
	drs      []Recommendation
	promoted map[string]time.Time
	fwPushed map[string]string
}

func New(store store.Secrets, metricsPath string, replications int) *Manager {
	return &Manager{store: store, st: state{lastSeen: map[string]time.Time{}, busy: map[string]bool{}, syncing: map[string]bool{}, promoted: map[string]time.Time{}, fwPushed: map[string]string{}}, started: time.Now(), inv: inventory{nodes: map[string]NodeState{}}, tel: newTelemetry(metricsPath), replications: make(chan struct{}, max(replications, 1))}
}

func (c *Manager) Nodes(ctx context.Context) ([]Node, error) {
	ns, _, err := c.load(ctx)
	return ns, err
}

func (c *Manager) NodesFor(ctx context.Context, org string) ([]Node, error) {
	ns, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []Node
	for _, n := range ns {
		if n.Org == org {
			out = append(out, n)
		}
	}
	return out, nil
}

func (c *Manager) NodeIn(ctx context.Context, org, name string) (Node, error) {
	n, err := c.Node(ctx, name)
	if err == nil && n.Org != org {
		return Node{}, fmt.Errorf("node %s not found", name)
	}
	return n, err
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
	var ns []Node
	rv, err := store.LoadJSON(ctx, c.store, nodesKey, "nodes", &ns)
	return ns, rv, err
}

func (c *Manager) save(ctx context.Context, ns []Node, rv string) error {
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	return store.SaveJSON(ctx, c.store, nodesKey, "nodes", rv, ns)
}

func (c *Manager) AddNode(ctx context.Context, n Node, fingerprint string) error {
	n.URL = strings.TrimRight(n.URL, "/")
	ca, err := fetchCA(ctx, n.URL)
	if err != nil {
		return err
	}
	if got := tlscert.Fingerprint(ca); !strings.EqualFold(got, strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", "")) {
		return fmt.Errorf("CA fingerprint mismatch: node presents %s", got)
	}
	n.CA = string(ca)
	if _, err := n.Info(ctx); err != nil {
		return err
	}
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	for _, x := range ns {
		if x.Name == n.Name || x.URL == n.URL {
			return errors.New("node already registered")
		}
	}
	if err := c.save(ctx, append(ns, n), rv); err != nil {
		return err
	}
	return c.PushMigrationNetwork(ctx)
}

func (c *Manager) UpdateNodeAccess(ctx context.Context, org, name, url, token, fingerprint string) error {
	url = strings.TrimRight(url, "/")
	ca, err := fetchCA(ctx, url)
	if err != nil {
		return err
	}
	if got := tlscert.Fingerprint(ca); !strings.EqualFold(got, strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", "")) {
		return fmt.Errorf("CA fingerprint mismatch: node presents %s", got)
	}
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ns, func(n Node) bool { return n.Name == name && n.Org == org })
	if i < 0 {
		return fmt.Errorf("node %s not found", name)
	}
	if slices.ContainsFunc(ns, func(n Node) bool { return n.Name != name && n.URL == url }) {
		return errors.New("another node uses this address")
	}
	n := ns[i]
	n.URL, n.CA = url, string(ca)
	if token != "" {
		n.Token = token
	}
	if _, err := n.Info(ctx); err != nil {
		return err
	}
	ns[i] = n
	if err := c.save(ctx, ns, rv); err != nil {
		return err
	}
	go c.Refresh(name)
	return nil
}

func (c *Manager) RotateToken(ctx context.Context, org, name string) error {
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ns, func(n Node) bool { return n.Name == name && n.Org == org })
	if i < 0 {
		return fmt.Errorf("node %s not found", name)
	}
	var out struct{ Name, Token, Old string }
	if err := ns[i].do(ctx, http.MethodPost, "/api/tokens/rotate", nil, &out); err != nil {
		return err
	}
	ns[i].Token = out.Token
	if _, err := ns[i].Info(ctx); err != nil {
		return fmt.Errorf("new token did not work, old token kept: %w", err)
	}
	if err := c.save(ctx, ns, rv); err != nil {
		return err
	}
	go c.Refresh(name)
	if err := ns[i].do(ctx, http.MethodPost, "/api/tokens/retire", wire.RetireTokenRequest{Name: out.Old}, nil); err != nil {
		return fmt.Errorf("new token saved, but the old token %s is still valid on %s: %w", out.Old, name, err)
	}
	return nil
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
	c := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		TLSClientConfig:     trustPinnedOrPublic(pool, n.URL),
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
	n.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s: %s%w", n.Name, e.Error, errRejected)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (n Node) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Del(auth.ActorHeader)
	if a := auth.Actor(req.Context()); a != "" {
		req.Header.Set(auth.ActorHeader, a)
	}
}

func (n Node) Proxy(ctx context.Context, method, path string, body io.Reader, h http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, n.URL+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	n.authorize(req)
	return n.client().Do(req)
}

func (n Node) streaming() *http.Client {
	return &http.Client{Transport: n.client().Transport, CheckRedirect: n.client().CheckRedirect}
}

func (n Node) Open(ctx context.Context, method, path string, h http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, n.URL+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	n.authorize(req)
	resp, err := n.streaming().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		defer resp.Body.Close()
		var e struct{ Error string }
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("%s: %s", n.Name, e.Error)
	}
	return resp, nil
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
	return n.do(ctx, http.MethodPost, APIPath("/api/vms/%s/%s/%s", ns, vm, action), nil, nil)
}

func (c *Manager) preflight(ctx context.Context, src, dst Node, ns, vm string) error {
	if dst, err := c.Node(ctx, dst.Name); err == nil && dst.Maintenance {
		return fmt.Errorf("%s is in maintenance mode", dst.Name)
	}
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
	free := map[string]int64{}
	for _, p := range st.Pools {
		if p.StorageClass != "" {
			free[p.StorageClass] = p.Free
		}
	}
	free[""] = free["zfs"]
	need := map[string]int64{}
	for _, d := range source.Disks {
		if _, ok := free[d.StorageClass]; !ok {
			return fmt.Errorf("disk %s uses storage pool %s, which %s does not have", d.Name, d.StorageClass, dst.Name)
		}
		need[d.StorageClass] += d.SizeBytes
	}
	for class, n := range need {
		if have := free[class]; have < n+n/10 {
			pool := class
			if pool == "" {
				pool = "zfs"
			}
			return fmt.Errorf("pool %s on %s has %d GiB free, %s needs %d GiB plus headroom", pool, dst.Name, have>>30, vm, n>>30)
		}
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
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	out := make([]Job, 0, len(c.st.jobs))
	for i := len(c.st.jobs) - 1; i >= 0; i-- {
		if c.st.jobs[i].Org == org {
			out = append(out, *c.st.jobs[i])
		}
	}
	return out
}

func (c *Manager) unprotected(ctx context.Context, src Node, vm string) error {
	ps, err := c.Protected(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(ps, func(p Protected) bool { return p.VM == vm && (p.Owner == src.Name || p.Replica == src.Name) }) {
		return fmt.Errorf("%s is protected by HA; remove protection or use a planned failover to move it", vm)
	}
	return nil
}

func (c *Manager) Migrate(ctx context.Context, src, dst Node, ns, vm string) error {
	if err := c.unprotected(ctx, src, vm); err != nil {
		return err
	}
	return c.startJob(ctx, &Job{Org: src.Org, Namespace: ns, VM: vm, From: src.Name, To: dst.Name}, 12*time.Hour, func(ctx context.Context, j *Job) error {
		return c.migrate(ctx, j, src, dst, ns, vm)
	})
}

func APIPath(format string, args ...string) string {
	esc := make([]any, len(args))
	for i, a := range args {
		esc[i] = url.PathEscape(a)
	}
	return fmt.Sprintf(format, esc...)
}

func vmPathIn(ns, vm, action string) string {
	if action == "" {
		return APIPath("/api/vms/%s/%s", ns, vm)
	}
	return APIPath("/api/vms/%s/%s/", ns, vm) + action
}

func (c *Manager) carry(ctx context.Context, src, dst Node, ns, vm string) (map[string]string, error) {
	v, err := vmState(ctx, src, ns, vm)
	if err != nil {
		return nil, err
	}
	target := ""
	for _, f := range strings.Fields(v.Backup) {
		if t, ok := strings.CutPrefix(f, "target="); ok {
			target = t
		}
	}
	if target != "" {
		var ts []wire.BackupTarget
		if err := dst.do(ctx, http.MethodGet, "/api/host/backup-targets", nil, &ts); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(ts, func(t wire.BackupTarget) bool { return t.Name == target }) {
			return nil, fmt.Errorf("%s backs up to %s, which %s does not have; add that target on %s or change the backup policy", vm, target, dst.Name, dst.Name)
		}
	}
	return v.Metadata(), nil
}

func (c *Manager) importAndWait(ctx context.Context, j *Job, dst Node, name, pool string, template bool, ex kube.Export, meta map[string]string) error {
	c.progress(j, "Importing")
	rollback := func(cause error) error {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		err := dst.do(cctx, http.MethodPost, vmPathIn(kube.Namespace, name, "abort"), wire.OperationRequest{Operation: j.ID}, nil)
		lifecycle.LogCleanup("import of "+name, dst.do(cctx, http.MethodPost, vmPathIn(kube.Namespace, name, "imported"), nil, nil))
		if err != nil {
			return fmt.Errorf("%w: %w; target cleanup failed: %w", errUnknown, cause, err)
		}
		return cause
	}
	if err := dst.do(ctx, http.MethodPost, "/api/import", wire.ImportRequest{Name: name, Operation: j.ID, Export: ex, Pool: pool, Template: template, Metadata: meta}, nil); errors.Is(err, errRejected) {
		return err
	} else if err != nil {
		return rollback(err)
	}
	if err := c.waitDisks(ctx, j, dst, name); err != nil {
		return rollback(err)
	}
	c.progress(j, "Cleaning up")
	return dst.do(ctx, http.MethodPost, vmPathIn(kube.Namespace, name, "imported"), nil, nil)
}

func nodeDisks(n Node, path string, body any) func(context.Context) ([]kube.DiskStatus, error) {
	return func(ctx context.Context) ([]kube.DiskStatus, error) {
		var disks []kube.DiskStatus
		method := http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
		return disks, n.do(ctx, method, path, body, &disks)
	}
}

func (c *Manager) waitDisks(ctx context.Context, j *Job, dst Node, name string) error {
	fetch := nodeDisks(dst, vmPathIn(kube.Namespace, name, "disks"), nil)
	list := func(ctx context.Context) ([]kube.DiskStatus, error) {
		disks, err := fetch(ctx)
		if err != nil && ctx.Err() == nil {
			c.progress(j, "Copying (waiting for "+dst.Name+")")
			return nil, nil
		}
		return disks, err
	}
	failed, err := kube.WaitDisks(ctx, 5*time.Second, list, func(disks []kube.DiskStatus) {
		var progress []string
		for _, d := range disks {
			progress = append(progress, d.Name+" "+d.Progress)
		}
		c.progress(j, "Copying "+strings.Join(progress, ", "))
	})
	if failed != "" {
		return fmt.Errorf("disk %s failed to copy", failed)
	}
	return err
}

func (c *Manager) migrate(ctx context.Context, j *Job, src, dst Node, ns, vm string) error {
	if err := c.preflight(ctx, src, dst, ns, vm); err != nil {
		return err
	}
	meta, err := c.carry(ctx, src, dst, ns, vm)
	if err != nil {
		return err
	}
	c.progress(j, "Exporting")
	var ex kube.Export
	if err := src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "export"), wire.ExportRequest{BaseURL: src.URL}, &ex); err != nil {
		return err
	}
	unexport := func() {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		if err := src.do(cctx, http.MethodPost, vmPathIn(ns, vm, "unexport"), nil, nil); err != nil {
			log.Printf("migrate %s: unexport: %v", vm, err)
		}
	}

	if err := c.importAndWait(ctx, j, dst, vm, "", false, ex, meta); err != nil {
		if !errors.Is(err, errUnknown) {
			unexport()
		}
		return err
	}
	unexport()
	return src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "delete"), nil, nil)
}

type rejected struct{}

func (rejected) Error() string { return "" }

var errRejected = rejected{}

func (n Node) Call(ctx context.Context, method, path string, in, out any) error {
	return n.do(ctx, method, path, in, out)
}

func (n Node) LocalIP(ctx context.Context) (string, error) {
	var local string
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		local, _, _ = net.SplitHostPort(i.Conn.LocalAddr().String())
	}})
	if err := n.do(ctx, http.MethodGet, "/api/host/status", nil, nil); err != nil {
		return "", err
	}
	return local, nil
}

func (n Node) DialConsole(ctx context.Context, ns, vm, kind string) (*websocket.Conn, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(n.CA))
	d := websocket.Dialer{TLSClientConfig: trustPinnedOrPublic(pool, n.URL), Subprotocols: []string{"binary"}}
	u := strings.Replace(n.URL, "https://", "wss://", 1) + APIPath("/api/vms/%s/%s/ws/%s", ns, vm, kind)
	conn, resp, err := d.DialContext(ctx, u, http.Header{"Authorization": {"Bearer " + n.Token}})
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil && resp != nil {
		return nil, fmt.Errorf("%s: console: %s", n.Name, resp.Status)
	}
	return conn, err
}

func trustPinnedOrPublic(pinned *x509.CertPool, rawURL string) *tls.Config {
	host := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Hostname()
	}
	return &tls.Config{
		ServerName:         tlscert.InternalName,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no server certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Intermediates: inter, Roots: pinned}); err == nil {
				return nil
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter})
			return err
		},
	}
}
