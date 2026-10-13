package managerserver

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/netguard"
	"argus/internal/registry"
	"argus/internal/tlscert"
	"argus/internal/ui"
	"argus/internal/wire"
)

const (
	registryKey = "argus-registry"
	sourcesKey  = "argus-image-sources-"
	pullUser    = "argus-pull"
	gib         = 1 << 30
)

var (
	tagRE     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	addressRE = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

type ImageSource struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
	CA   string `json:"ca,omitempty"`
}

type libraryImage struct {
	registry.Image
	Name     string
	HostName string
	SizeGiB  int
}

type registryAuth struct {
	mu    sync.Mutex
	pulls map[string]pullGrant
	users map[string]userGrant
	ips   map[string]bool
}

type pullGrant struct {
	repo    string
	expires time.Time
}

type userGrant struct {
	role    auth.Role
	scoped  bool
	expires time.Time
}

func secretKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Server) startRegistry(ctx context.Context) error {
	g, err := registry.New(filepath.Join(s.dataDir, "registry"))
	if err != nil {
		return err
	}
	g.Authorize = s.registryAuthorize
	s.registry = g
	s.regAuth = registryAuth{pulls: map[string]pullGrant{}, users: map[string]userGrant{}, ips: map[string]bool{}}
	if err := s.reloadManagerCert(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Server) registryAddress(ctx context.Context) string {
	var st struct{ Address string }
	if _, err := s.LoadJSON(ctx, registryKey, &st); err != nil {
		log.Printf("registry address: %v", err)
	}
	return st.Address
}

func (s *Server) hostRegistry(ctx context.Context, n manager.Node, trust bool) (string, error) {
	if addr := s.registryAddress(ctx); addr != "" {
		return addr, nil
	}
	ip, err := n.LocalIP(ctx)
	if err == nil && net.ParseIP(ip) == nil {
		err = errors.New("no local address")
	}
	if err != nil {
		return "", fmt.Errorf("cannot find the address %s uses to reach Argus Manager: %w", n.Name, err)
	}
	s.regAuth.mu.Lock()
	known := s.regAuth.ips[ip] || !trust
	s.regAuth.ips[ip] = s.regAuth.ips[ip] || trust
	s.regAuth.mu.Unlock()
	if !known {
		if err := s.reloadManagerCert(ctx); err != nil {
			return "", err
		}
	}
	if s.port == "443" {
		return strings.TrimSuffix(net.JoinHostPort(ip, "443"), ":443"), nil
	}
	return net.JoinHostPort(ip, s.port), nil
}

func (s *Server) reloadManagerCert(ctx context.Context) error {
	host := s.registryAddress(ctx)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var ips []net.IP
	var names []string
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else if host != "" {
		names = append(names, host)
	}
	s.regAuth.mu.Lock()
	for ip := range s.regAuth.ips {
		ips = append(ips, net.ParseIP(ip))
	}
	s.regAuth.mu.Unlock()
	pki, err := tlscert.Load(ctx, s.Auth.Secrets(), ips, names...)
	if err != nil {
		return err
	}
	s.Certs.SetBuiltin(pki.Cert)
	s.caPEM = pki.CAPEM
	return nil
}

func (s *Server) newPull(repo string) (string, string) {
	b := make([]byte, 24)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	s.regAuth.mu.Lock()
	defer s.regAuth.mu.Unlock()
	for k, g := range s.regAuth.pulls {
		if time.Now().After(g.expires) {
			delete(s.regAuth.pulls, k)
		}
	}
	s.regAuth.pulls[secretKey(tok)] = pullGrant{repo: repo, expires: time.Now().Add(12 * time.Hour)}
	return pullUser, tok
}

func (s *Server) registryUser(r *http.Request) (org, name string) {
	user, _, _ := r.BasicAuth()
	org, name = manager.DefaultOrg, user
	if o, n, ok := strings.Cut(user, "/"); ok {
		org, name = o, n
	}
	return org, name
}

func (s *Server) registryAuthorize(r *http.Request, repo string, a registry.Access) bool {
	user, pass, ok := r.BasicAuth()
	if !ok || pass == "" {
		return false
	}
	if user == pullUser {
		s.regAuth.mu.Lock()
		g, ok := s.regAuth.pulls[secretKey(pass)]
		s.regAuth.mu.Unlock()
		return ok && time.Now().Before(g.expires) && a == registry.Pull && (repo == "" || repo == g.repo)
	}
	org, name := s.registryUser(r)
	if repo != "" && !strings.HasPrefix(repo, org+"/") {
		return false
	}
	key := secretKey(org, name, pass)
	s.regAuth.mu.Lock()
	g, ok := s.regAuth.users[key]
	s.regAuth.mu.Unlock()
	if !ok || time.Now().After(g.expires) {
		ip := ui.ClientIP(r)
		if s.Limiter.Blocked(ip) {
			return false
		}
		u, ok, err := s.Auth.ForOrg(org).Verify(r.Context(), name, pass)
		if !ok {
			if err == nil {
				s.Limiter.Fail(ip)
			}
			return false
		}
		s.Limiter.Succeed(ip)
		g = userGrant{role: u.Role, scoped: len(u.Scopes) > 0, expires: time.Now().Add(5 * time.Minute)}
		s.regAuth.mu.Lock()
		s.regAuth.users[key] = g
		s.regAuth.mu.Unlock()
	}
	return a == registry.Pull || g.role.Can(auth.Operator) && !g.scoped
}

func (s *Server) serveRegistry(w http.ResponseWriter, r *http.Request) {
	if (r.Method == http.MethodPut || r.Method == http.MethodDelete) && strings.Contains(r.URL.Path, "/manifests/") {
		status := ui.RecordStatus(w, func(w http.ResponseWriter) { s.registry.ServeHTTP(w, r) })
		org, name := s.registryUser(r)
		s.Record(s.AuditUser(org, name), "registry "+r.Method+" "+strings.TrimPrefix(r.URL.Path, "/v2/"), status < 400, "")
		return
	}
	s.registry.ServeHTTP(w, r)
}

func (s *Server) library(org string) []libraryImage {
	var out []libraryImage
	for _, img := range s.registry.List(org + "/") {
		name := strings.TrimPrefix(img.Repo, org+"/")
		host := strings.NewReplacer(".", "-", "_", "-", "/", "-").Replace(strings.ToLower(name + "-" + img.Tag))
		out = append(out, libraryImage{Image: img, Name: name, HostName: strings.Trim(host, "-"), SizeGiB: max(10, int(math.Ceil(float64(img.Size)/gib))+1)})
	}
	return out
}

func (s *Server) imageSources(ctx context.Context, org string) ([]ImageSource, string, error) {
	var out []ImageSource
	rv, err := s.LoadJSON(ctx, sourcesKey+org, &out)
	return out, rv, err
}

func freeSpace(dir string, need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	if free := int64(st.Bavail) * int64(st.Bsize); need > 0 && free < 2*need+gib {
		return fmt.Errorf("only %.1f GiB free on Argus Manager; this image needs about %.1f GiB while it is stored", float64(free)/gib, float64(2*need)/gib)
	}
	return nil
}

func (s *Server) storeDisk(org, name, tag, file, arch string, src io.Reader) error {
	switch {
	case !ui.ValidObject.MatchString(name):
		return errors.New("name must be lowercase letters, digits and dashes")
	case !tagRE.MatchString(tag):
		return errors.New("invalid tag")
	case strings.HasSuffix(file, ".xz"):
		return errors.New("xz-compressed images are not supported; decompress it first")
	case strings.HasSuffix(file, ".gz"):
		zr, err := gzip.NewReader(src)
		if err != nil {
			return err
		}
		defer zr.Close()
		src, file = zr, strings.TrimSuffix(file, ".gz")
	}
	tmp, err := os.CreateTemp(s.dataDir, "library-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, src)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if file == "" || file == "." {
		file = name + ".img"
	}
	_, err = s.registry.PutDisk(org+"/"+name, tag, file, arch, n, tmp)
	return err
}

type progressReader struct {
	r    io.Reader
	n    int64
	last time.Time
	fn   func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if time.Since(p.last) > 5*time.Second {
		p.last = time.Now()
		p.fn(p.n)
	}
	return n, err
}

func (s *Server) libraryAdd(r *http.Request) error {
	org := ui.Session(r).Org
	mr, err := r.MultipartReader()
	if err != nil {
		return err
	}
	vals := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if part.FormName() == "file" {
			if vals["via"] == "url" || part.FileName() == "" {
				continue
			}
			if err := freeSpace(s.dataDir, r.ContentLength); err != nil {
				return err
			}
			return s.storeDisk(org, vals["name"], vals["tag"], filepath.Base(part.FileName()), vals["arch"], part)
		}
		b, _ := io.ReadAll(io.LimitReader(part, 4096))
		vals[part.FormName()] = strings.TrimSpace(string(b))
	}
	name, tag, src, arch := vals["name"], vals["tag"], vals["url"], vals["arch"]
	switch {
	case vals["via"] != "url":
		return errors.New("choose a file to upload")
	case !strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "http://"):
		return errors.New("enter an http or https URL")
	case !ui.ValidObject.MatchString(name) || !tagRE.MatchString(tag):
		return errors.New("invalid name or tag")
	}
	return s.manager.Task(r.Context(), org, "Import "+name+":"+tag+" to library", "library:"+org+"/"+name, func(ctx context.Context, progress func(string)) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return err
		}
		resp, err := netguard.Client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s returned %s", src, resp.Status)
		}
		if err := freeSpace(s.dataDir, resp.ContentLength); err != nil {
			return err
		}
		pr := &progressReader{r: resp.Body, last: time.Now(), fn: func(n int64) { progress(fmt.Sprintf("Downloaded %.1f GiB", float64(n)/gib)) }}
		progress("Downloading")
		if err := s.storeDisk(org, name, tag, filepath.Base(req.URL.Path), arch, pr); err != nil {
			return err
		}
		progress("Stored")
		return nil
	})
}

func (s *Server) nodesNamed(r *http.Request, names []string) ([]manager.Node, error) {
	var out []manager.Node
	for _, h := range names {
		n, err := s.nodeIn(r, h)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("pick at least one host")
	}
	return out, nil
}

func (s *Server) librarySend(r *http.Request) error {
	org := ui.Session(r).Org
	name, tag, _ := strings.Cut(r.FormValue("image"), ":")
	hostName := r.FormValue("name")
	size, err := ui.Gibibytes(r.FormValue("size"))
	if err != nil {
		return err
	}
	if !ui.ValidObject.MatchString(hostName) {
		return errors.New("name on hosts must be lowercase letters, digits and dashes")
	}
	if !slices.ContainsFunc(s.library(org), func(i libraryImage) bool { return i.Name == name && i.Tag == tag }) {
		return fmt.Errorf("%s is not in the library", r.FormValue("image"))
	}
	targets, err := s.nodesNamed(r, r.Form["hosts"])
	if err != nil {
		return err
	}
	return s.manager.SendImage(r.Context(), org, hostName, size, targets, func(ctx context.Context, t manager.Node) (kube.RegistrySource, error) {
		return s.librarySource(ctx, t, name, tag)
	})
}

func (s *Server) librarySource(ctx context.Context, n manager.Node, name, tag string) (kube.RegistrySource, error) {
	addr, err := s.hostRegistry(ctx, n, true)
	if err != nil {
		return kube.RegistrySource{}, err
	}
	repo := n.Org + "/" + name
	src := kube.RegistrySource{Image: addr + "/" + repo + ":" + tag, CA: string(s.caPEM)}
	src.User, src.Pass = s.newPull(repo)
	return src, nil
}

func (s *Server) importFromSource(r *http.Request) error {
	org := ui.Session(r).Org
	size, err := ui.Gibibytes(r.FormValue("size"))
	if err != nil {
		return err
	}
	name, path := r.FormValue("name"), strings.TrimSpace(r.FormValue("path"))
	if !ui.ValidObject.MatchString(name) {
		return errors.New("invalid name")
	}
	req := wire.ImageImportRequest{Name: name, Size: size}
	from := "URL"
	switch src := r.FormValue("source"); src {
	case "":
		req.URL = path
	default:
		sources, _, err := s.imageSources(r.Context(), org)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(sources, func(x ImageSource) bool { return x.Name == src })
		if i < 0 {
			return fmt.Errorf("source %s not found", src)
		}
		from = src
		base := strings.TrimRight(sources[i].URL, "/") + "/" + strings.TrimLeft(path, "/")
		if sources[i].Kind == "registry" {
			req.Registry = &kube.RegistrySource{Image: base, CA: sources[i].CA}
		} else {
			req.URL = base
		}
	}
	if req.Registry == nil && !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
		return errors.New("enter an http or https URL")
	}
	targets, err := s.nodesNamed(r, r.Form["hosts"])
	if err != nil {
		return err
	}
	return s.manager.ImportImage(r.Context(), org, from, req, targets)
}

func (s *Server) changeSources(r *http.Request, add bool) error {
	org := ui.Session(r).Org
	sources, rv, err := s.imageSources(r.Context(), org)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(r.FormValue("name"))
	sources = slices.DeleteFunc(sources, func(x ImageSource) bool { return x.Name == name })
	if add {
		src := ImageSource{Name: name, Kind: r.FormValue("kind"), URL: strings.TrimSpace(r.FormValue("url")), CA: strings.TrimSpace(r.FormValue("ca"))}
		switch {
		case name == "" || len(name) > 64:
			return errors.New("enter a name")
		case src.Kind == "registry" && (strings.Contains(src.URL, "://") || src.URL == ""):
			return errors.New("enter a registry as host[:port][/path], without a scheme")
		case src.Kind == "http" && !strings.HasPrefix(src.URL, "https://") && !strings.HasPrefix(src.URL, "http://"):
			return errors.New("enter an http or https base URL")
		case src.Kind != "registry" && src.Kind != "http":
			return errors.New("unknown source type")
		case src.CA != "" && !strings.Contains(src.CA, "BEGIN CERTIFICATE"):
			return errors.New("CA must be a PEM certificate")
		}
		sources = append(sources, src)
	}
	return s.SaveJSON(r.Context(), sourcesKey+org, rv, sources)
}

func (s *Server) setRegistryAddress(r *http.Request) error {
	if !superAdmin(r) {
		return errors.New("only an administrator of the default organization can change this")
	}
	addr := strings.TrimSpace(r.FormValue("address"))
	if addr != "" && !addressRE.MatchString(addr) {
		return errors.New("enter a host name or IP address, optionally with :port")
	}
	var st struct{ Address string }
	rv, err := s.LoadJSON(r.Context(), registryKey, &st)
	if err != nil {
		return err
	}
	st.Address = addr
	if err := s.SaveJSON(r.Context(), registryKey, rv, st); err != nil {
		return err
	}
	return s.reloadManagerCert(r.Context())
}
