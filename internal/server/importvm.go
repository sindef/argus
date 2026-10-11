package server

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"argus/internal/kube"
)

type ovfEnvelope struct {
	References []struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	} `xml:"References>File"`
	Disks []struct {
		ID       string `xml:"diskId,attr"`
		FileRef  string `xml:"fileRef,attr"`
		Capacity string `xml:"capacity,attr"`
		Units    string `xml:"capacityAllocationUnits,attr"`
	} `xml:"DiskSection>Disk"`
	System struct {
		Name  string `xml:"Name"`
		Items []struct {
			Type     int    `xml:"ResourceType"`
			Quantity int64  `xml:"VirtualQuantity"`
			Units    string `xml:"AllocationUnits"`
		} `xml:"VirtualHardwareSection>Item"`
		Config []struct {
			Key   string `xml:"key,attr"`
			Value string `xml:"value,attr"`
		} `xml:"VirtualHardwareSection>Config"`
	} `xml:"VirtualSystem"`
}

var (
	unitPower = regexp.MustCompile(`2\s*\^\s*(\d+)`)
	nonName   = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
)

func ovfBytes(n int64, units string) int64 {
	if m := unitPower.FindStringSubmatch(units); m != nil {
		p, _ := strconv.Atoi(m[1])
		return n << p
	}
	return n
}

func (s *Server) importOVA(r *http.Request) (string, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", err
	}
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return "", errors.New("no OVA file in the request")
		}
		if part.FormName() != "ova" {
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			fields[part.FormName()] = strings.TrimSpace(string(b))
			continue
		}
		return s.streamOVA(r.Context(), fields, part)
	}
}

func (s *Server) streamOVA(ctx context.Context, fields map[string]string, body io.Reader) (string, error) {
	tr := tar.NewReader(body)
	hdr, err := tr.Next()
	if err != nil || !strings.HasSuffix(strings.ToLower(hdr.Name), ".ovf") {
		return "", errors.New("not an OVA: the first entry must be the .ovf descriptor")
	}
	var env ovfEnvelope
	if err := xml.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&env); err != nil {
		return "", fmt.Errorf("invalid OVF: %w", err)
	}
	class, err := s.poolClass(ctx, fields["pool"])
	if err != nil {
		return "", err
	}
	v := kube.ImportedVM{Name: fields["name"], Network: fields["network"], Class: class, Cores: 1, MemoryMiB: 1024}
	if v.Name == "" {
		v.Name = strings.Trim(strings.ToLower(nonName.ReplaceAllString(env.System.Name, "-")), "-")
	}
	if !validObject.MatchString(v.Name) || v.Network == "" {
		return "", errors.New("valid VM name and network required")
	}
	for _, it := range env.System.Items {
		switch it.Type {
		case 3:
			v.Cores = it.Quantity
		case 4:
			v.MemoryMiB = ovfBytes(it.Quantity, it.Units) >> 20
		}
	}
	for _, c := range env.System.Config {
		if c.Key == "firmware" {
			v.Firmware = c.Value
		}
	}
	files := map[string]string{}
	for _, f := range env.References {
		files[f.ID] = f.Href
	}
	order := map[string]int{}
	for _, d := range env.Disks {
		capacity, _ := strconv.ParseInt(d.Capacity, 10, 64)
		order[files[d.FileRef]] = len(v.Disks)
		v.Disks = append(v.Disks, kube.ImportedDisk{SizeBytes: ovfBytes(capacity, d.Units), Source: map[string]any{"upload": map[string]any{}}})
	}
	dvs, err := s.kube.CreateImportedVM(ctx, v)
	if err != nil {
		return "", err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return v.Name, err
		}
		i, ok := order[path.Base(hdr.Name)]
		if !ok {
			continue
		}
		if err := s.kube.Upload(ctx, dvs[i], tr); err != nil {
			return v.Name, fmt.Errorf("disk %s: %w", hdr.Name, err)
		}
		delete(order, path.Base(hdr.Name))
	}
	if len(order) > 0 {
		return v.Name, errors.New("the OVA is missing disk files listed in its descriptor")
	}
	return v.Name, nil
}

type esxiSource struct {
	Host, User, Password, DC, Datastore, Dir string
	pin                                      [32]byte
}

type esxiStreams struct {
	mu sync.Mutex
	m  map[string]esxiStream
}

type esxiStream struct {
	src   *esxiSource
	files []string
}

var (
	vmxPath = regexp.MustCompile(`^\[([^\]]+)\]\s*(.+\.vmx)$`)
	vmxDisk = regexp.MustCompile(`^((?:scsi|sata|nvme|ide)\d+:\d+)\.filename$`)
	extent  = regexp.MustCompile(`^RW\s+(\d+)\s+(?:VMFS|FLAT)\s+"([^"]+)"`)
)

func (e *esxiSource) url(file string) string {
	q := url.Values{"dcPath": {e.DC}, "dsName": {e.Datastore}}
	return "https://" + e.Host + "/folder/" + (&url.URL{Path: path.Join(e.Dir, file)}).EscapedPath() + "?" + q.Encode()
}

func (e *esxiSource) pinCert() error {
	addr := e.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr += ":443"
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	defer conn.Close()
	e.pin = sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return nil
}

func (e *esxiSource) client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{ResponseHeaderTimeout: time.Minute, TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || sha256.Sum256(raw[0]) != e.pin {
				return errors.New("ESXi certificate changed since the import started")
			}
			return nil
		},
	}}}
}

func (e *esxiSource) get(ctx context.Context, file string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url(file), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(e.User, e.Password)
	resp, err := e.client(30 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", file, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

func keyValues(text string) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return kv
}

func flatExtent(desc string) (int64, string) {
	var size int64
	flat := ""
	for _, line := range strings.Split(desc, "\n") {
		if em := extent.FindStringSubmatch(strings.TrimSpace(line)); em != nil {
			sectors, _ := strconv.ParseInt(em[1], 10, 64)
			size, flat = size+sectors*512, em[2]
		}
	}
	return size, flat
}

func sortSlots(slots []string) {
	rank := func(s string) string {
		for i, p := range []string{"scsi", "sata", "nvme", "ide"} {
			if strings.HasPrefix(s, p) {
				return strconv.Itoa(i) + s
			}
		}
		return s
	}
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0 && rank(slots[j]) < rank(slots[j-1]); j-- {
			slots[j], slots[j-1] = slots[j-1], slots[j]
		}
	}
}

func (s *Server) importESXi(ctx context.Context, f func(string) string) (string, error) {
	m := vmxPath.FindStringSubmatch(strings.TrimSpace(f("vmx")))
	if m == nil {
		return "", errors.New(`VM file must look like "[datastore1] web01/web01.vmx"`)
	}
	e := &esxiSource{Host: strings.TrimSpace(f("host")), User: f("user"), Password: f("password"), DC: f("dc"), Datastore: m[1], Dir: path.Dir(m[2])}
	if e.DC == "" {
		e.DC = "ha-datacenter"
	}
	if e.Host == "" {
		return "", errors.New("ESXi host required")
	}
	if err := e.pinCert(); err != nil {
		return "", err
	}
	seen := hex.EncodeToString(e.pin[:])
	want := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(f("fingerprint")))
	if want != seen {
		return "", fmt.Errorf("confirm the ESXi certificate fingerprint before credentials are sent; this host presents %s", seen)
	}
	vmx, err := e.get(ctx, path.Base(m[2]))
	if err != nil {
		return "", err
	}
	kv := keyValues(vmx)
	class, err := s.poolClass(ctx, f("pool"))
	if err != nil {
		return "", err
	}
	v := kube.ImportedVM{Name: f("name"), Network: f("network"), Class: class, Cores: 1, MemoryMiB: 1024, Firmware: kv["firmware"]}
	if v.Name == "" {
		v.Name = strings.Trim(strings.ToLower(nonName.ReplaceAllString(kv["displayname"], "-")), "-")
	}
	if !validObject.MatchString(v.Name) || v.Network == "" {
		return "", errors.New("valid VM name and network required")
	}
	if n, err := strconv.ParseInt(kv["numvcpus"], 10, 64); err == nil {
		v.Cores = n
	}
	if n, err := strconv.ParseInt(kv["memsize"], 10, 64); err == nil {
		v.MemoryMiB = n
	}
	var slots []string
	for k := range kv {
		if dm := vmxDisk.FindStringSubmatch(k); dm != nil && !strings.EqualFold(kv[dm[1]+".present"], "false") && !strings.Contains(kv[dm[1]+".devicetype"], "cdrom") && strings.HasSuffix(strings.ToLower(kv[k]), ".vmdk") {
			slots = append(slots, dm[1])
		}
	}
	if len(slots) == 0 {
		return "", errors.New("no disks found in the .vmx")
	}
	sortSlots(slots)
	stream := esxiStream{src: e}
	for _, slot := range slots {
		desc, err := e.get(ctx, kv[slot+".filename"])
		if err != nil {
			return "", err
		}
		if strings.Contains(desc, "parentFileNameHint") {
			return "", errors.New(kv[slot+".filename"] + " has snapshots; consolidate them on ESXi first")
		}
		size, flat := flatExtent(desc)
		if flat == "" {
			return "", errors.New(kv[slot+".filename"] + " is not a flat VMFS disk")
		}
		stream.files = append(stream.files, flat)
		v.Disks = append(v.Disks, kube.ImportedDisk{SizeBytes: size})
	}
	b := make([]byte, 24)
	rand.Read(b)
	token := hex.EncodeToString(b)
	secret := "argus-esxi-" + v.Name
	for i := range v.Disks {
		v.Disks[i].Source = map[string]any{"http": map[string]any{"url": fmt.Sprintf("https://%s/esxi-import/%d", os.Getenv("HOST_IP"), i), "certConfigMap": secret, "secretExtraHeaders": []any{secret}}}
	}
	if err := s.kube.CreateTokenSource(ctx, secret, string(s.caPEM), token); err != nil {
		return "", err
	}
	s.esxi.mu.Lock()
	if s.esxi.m == nil {
		s.esxi.m = map[string]esxiStream{}
	}
	s.esxi.m[token] = stream
	s.esxi.mu.Unlock()
	cleanup := func() {
		s.kube.DeleteTokenSource(context.Background(), secret)
		s.esxi.mu.Lock()
		delete(s.esxi.m, token)
		s.esxi.mu.Unlock()
	}
	dvs, err := s.kube.CreateImportedVM(ctx, v)
	if err != nil {
		cleanup()
		return "", err
	}
	go func() {
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		for ctx.Err() == nil {
			done := true
			for _, dv := range dvs {
				phase, _ := s.kube.DataVolumePhase(ctx, kube.Namespace, dv)
				done = done && (phase == "Succeeded" || phase == "Failed")
			}
			if done {
				log.Printf("import of %s from ESXi %s finished", v.Name, e.Host)
				return
			}
			time.Sleep(15 * time.Second)
		}
	}()
	return v.Name, nil
}

func (s *Server) serveESXi(w http.ResponseWriter, r *http.Request) {
	s.esxi.mu.Lock()
	st, ok := s.esxi.m[strings.TrimSpace(r.Header.Get("x-kubevirt-export-token"))]
	s.esxi.mu.Unlock()
	i, err := strconv.Atoi(r.PathValue("disk"))
	if !ok || err != nil || i < 0 || i >= len(st.files) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, st.src.url(st.files[i]), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.SetBasicAuth(st.src.User, st.src.Password)
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := st.src.client(0).Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Length", "Content-Type", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	log.Printf("esxi import %s: %s %d bytes", st.files[i], r.Method, n)
}

func (s *Server) importModal(w http.ResponseWriter, r *http.Request) {
	nets, _ := s.kube.ListNetworks(r.Context())
	s.render(w, r, "vmnew.html", "import-modal", map[string]any{"Networks": nets, "Pools": s.poolChoices(r.Context())})
}

func (s *Server) importVM(w http.ResponseWriter, r *http.Request) {
	var name string
	var err error
	if r.PathValue("source") == "ova" {
		name, err = s.importOVA(r)
	} else {
		name, err = s.importESXi(r.Context(), r.FormValue)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: import vm %s from %s", session(r).User, name, r.PathValue("source"))
	if base, ok := r.Context().Value(baseKey{}).(string); ok {
		redirect(w, r, strings.Replace(base, "/hosts/", "/nodes/", 1)+"/vms/"+kube.Namespace+"/"+name)
		return
	}
	redirect(w, r, "/vms/"+kube.Namespace+"/"+name)
}
