package vsphere

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Ref struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

func (r Ref) xml(tag string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(r.Value))
	return fmt.Sprintf(`<urn:%s type="%s">%s</urn:%s>`, tag, r.Type, b.String(), tag)
}

type Client struct {
	url  string
	hc   *http.Client
	sc   serviceContent
	host string
}

type serviceContent struct {
	RootFolder        Ref `xml:"rootFolder"`
	PropertyCollector Ref `xml:"propertyCollector"`
	ViewManager       Ref `xml:"viewManager"`
	SessionManager    Ref `xml:"sessionManager"`
}

type Fault struct{ Message string }

func (f Fault) Error() string { return f.Message }

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func Dial(ctx context.Context, host, user, password string, tlsConfig *tls.Config) (*Client, error) {
	jar, _ := cookiejar.New(nil)
	c := &Client{url: "https://" + host + "/sdk", host: host, hc: &http.Client{Jar: jar, Timeout: 2 * time.Minute, Transport: &http.Transport{TLSClientConfig: tlsConfig}}}
	var sc struct {
		Returnval serviceContent `xml:"Body>RetrieveServiceContentResponse>returnval"`
	}
	if err := c.call(ctx, `<urn:RetrieveServiceContent><urn:_this type="ServiceInstance">ServiceInstance</urn:_this></urn:RetrieveServiceContent>`, &sc); err != nil {
		return nil, err
	}
	c.sc = sc.Returnval
	if err := c.call(ctx, `<urn:Login>`+c.sc.SessionManager.xml("_this")+`<urn:userName>`+esc(user)+`</urn:userName><urn:password>`+esc(password)+`</urn:password></urn:Login>`, nil); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Logout() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.call(ctx, `<urn:Logout>`+c.sc.SessionManager.xml("_this")+`</urn:Logout>`, nil)
}

func (c *Client) call(ctx context.Context, body string, out any) error {
	env := `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:urn="urn:vim25" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body>` + body + `</soapenv:Body></soapenv:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(env))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", "urn:vim25/7.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	var f struct {
		Fault string `xml:"Body>Fault>faultstring"`
	}
	if xml.Unmarshal(b, &f) == nil && f.Fault != "" {
		return Fault{f.Fault}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vSphere returned %s", resp.Status)
	}
	if out != nil {
		return xml.Unmarshal(b, out)
	}
	return nil
}

type prop struct {
	Name string `xml:"name"`
	Val  struct {
		Inner string `xml:",innerxml"`
	} `xml:"val"`
}

type object struct {
	Obj   Ref    `xml:"obj"`
	Props []prop `xml:"propSet"`
}

func (o object) get(name string) string {
	for _, p := range o.Props {
		if p.Name == name {
			return p.Val.Inner
		}
	}
	return ""
}

type retrieveResult struct {
	Objects []object `xml:"objects"`
	Token   string   `xml:"token"`
}

func (c *Client) retrieve(ctx context.Context, objectSet, typ string, paths ...string) ([]object, error) {
	var ps strings.Builder
	for _, p := range paths {
		ps.WriteString("<urn:pathSet>" + p + "</urn:pathSet>")
	}
	var out []object
	var res struct {
		R retrieveResult `xml:"Body>RetrievePropertiesExResponse>returnval"`
	}
	err := c.call(ctx, `<urn:RetrievePropertiesEx>`+c.sc.PropertyCollector.xml("_this")+`<urn:specSet><urn:propSet><urn:type>`+typ+`</urn:type>`+ps.String()+`</urn:propSet>`+objectSet+`</urn:specSet><urn:options><urn:maxObjects>500</urn:maxObjects></urn:options></urn:RetrievePropertiesEx>`, &res)
	for err == nil {
		out = append(out, res.R.Objects...)
		if res.R.Token == "" {
			break
		}
		var next struct {
			R retrieveResult `xml:"Body>ContinueRetrievePropertiesExResponse>returnval"`
		}
		err = c.call(ctx, `<urn:ContinueRetrievePropertiesEx>`+c.sc.PropertyCollector.xml("_this")+`<urn:token>`+esc(res.R.Token)+`</urn:token></urn:ContinueRetrievePropertiesEx>`, &next)
		res.R = next.R
	}
	return out, err
}

func (c *Client) props(ctx context.Context, obj Ref, paths ...string) (object, error) {
	objs, err := c.retrieve(ctx, `<urn:objectSet>`+obj.xml("obj")+`<urn:skip>false</urn:skip></urn:objectSet>`, obj.Type, paths...)
	if err != nil {
		return object{}, err
	}
	if len(objs) == 0 {
		return object{}, fmt.Errorf("%s not found", obj.Value)
	}
	return objs[0], nil
}

type VM struct {
	Ref   string `json:"ref"`
	Name  string `json:"name"`
	Power string `json:"power"`
}

func (c *Client) VMs(ctx context.Context) ([]VM, error) {
	var view struct {
		R Ref `xml:"Body>CreateContainerViewResponse>returnval"`
	}
	if err := c.call(ctx, `<urn:CreateContainerView>`+c.sc.ViewManager.xml("_this")+c.sc.RootFolder.xml("container")+`<urn:type>VirtualMachine</urn:type><urn:recursive>true</urn:recursive></urn:CreateContainerView>`, &view); err != nil {
		return nil, err
	}
	defer c.call(context.Background(), `<urn:DestroyView>`+view.R.xml("_this")+`</urn:DestroyView>`, nil)
	objs, err := c.retrieve(ctx, `<urn:objectSet>`+view.R.xml("obj")+`<urn:skip>true</urn:skip><urn:selectSet xsi:type="urn:TraversalSpec"><urn:name>view</urn:name><urn:type>ContainerView</urn:type><urn:path>view</urn:path><urn:skip>false</urn:skip></urn:selectSet></urn:objectSet>`,
		"VirtualMachine", "name", "runtime.powerState", "config.template")
	if err != nil {
		return nil, err
	}
	var out []VM
	for _, o := range objs {
		if o.get("config.template") != "true" {
			out = append(out, VM{Ref: o.Obj.Value, Name: o.get("name"), Power: o.get("runtime.powerState")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type Disk struct {
	Key      int
	Capacity int64
}

type NIC struct {
	Type string
	MAC  string
}

type Config struct {
	Name      string
	Cores     int64
	MemoryMiB int64
	Firmware  string
	GuestID   string
	Power     string
	Disks     []Disk
	NICs      []NIC
}

func (c *Client) Config(ctx context.Context, vm string) (Config, error) {
	o, err := c.props(ctx, Ref{"VirtualMachine", vm}, "config.name", "config.hardware", "config.firmware", "config.guestId", "runtime.powerState")
	if err != nil {
		return Config{}, err
	}
	var hw struct {
		NumCPU   int64 `xml:"numCPU"`
		MemoryMB int64 `xml:"memoryMB"`
		Device   []struct {
			Type            string `xml:"type,attr"`
			Key             int    `xml:"key"`
			CapacityInBytes int64  `xml:"capacityInBytes"`
			CapacityInKB    int64  `xml:"capacityInKB"`
			MacAddress      string `xml:"macAddress"`
		} `xml:"device"`
	}
	if err := xml.Unmarshal([]byte("<hw>"+o.get("config.hardware")+"</hw>"), &hw); err != nil {
		return Config{}, fmt.Errorf("vm hardware: %w", err)
	}
	cfg := Config{Name: o.get("config.name"), Cores: hw.NumCPU, MemoryMiB: hw.MemoryMB, Firmware: o.get("config.firmware"), GuestID: o.get("config.guestId"), Power: o.get("runtime.powerState")}
	for _, d := range hw.Device {
		switch {
		case d.Type == "VirtualDisk":
			size := d.CapacityInBytes
			if size == 0 {
				size = d.CapacityInKB << 10
			}
			cfg.Disks = append(cfg.Disks, Disk{d.Key, size})
		case d.MacAddress != "":
			cfg.NICs = append(cfg.NICs, NIC{strings.TrimPrefix(d.Type, "Virtual"), d.MacAddress})
		}
	}
	sort.Slice(cfg.Disks, func(i, j int) bool { return cfg.Disks[i].Key < cfg.Disks[j].Key })
	return cfg, nil
}

func (c *Client) wait(ctx context.Context, task Ref) (string, error) {
	for {
		o, err := c.props(ctx, task, "info.state", "info.result", "info.error.localizedMessage")
		if err != nil {
			return "", err
		}
		switch o.get("info.state") {
		case "success":
			return o.get("info.result"), nil
		case "error":
			msg := o.get("info.error.localizedMessage")
			if msg == "" {
				msg = "task failed"
			}
			return "", errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) Snapshot(ctx context.Context, vm, name string) (string, error) {
	var t struct {
		R Ref `xml:"Body>CreateSnapshot_TaskResponse>returnval"`
	}
	if err := c.call(ctx, `<urn:CreateSnapshot_Task>`+Ref{"VirtualMachine", vm}.xml("_this")+`<urn:name>`+esc(name)+`</urn:name><urn:memory>false</urn:memory><urn:quiesce>false</urn:quiesce></urn:CreateSnapshot_Task>`, &t); err != nil {
		return "", err
	}
	return c.wait(ctx, t.R)
}

func (c *Client) RemoveSnapshot(ctx context.Context, snapshot string) error {
	var t struct {
		R Ref `xml:"Body>RemoveSnapshot_TaskResponse>returnval"`
	}
	if err := c.call(ctx, `<urn:RemoveSnapshot_Task>`+Ref{"VirtualMachineSnapshot", snapshot}.xml("_this")+`<urn:removeChildren>false</urn:removeChildren><urn:consolidate>true</urn:consolidate></urn:RemoveSnapshot_Task>`, &t); err != nil {
		return err
	}
	_, err := c.wait(ctx, t.R)
	return err
}

type DiskURL struct {
	URL        string
	Thumbprint string
}

type Lease struct {
	c     *Client
	ref   Ref
	Disks []DiskURL
}

func (c *Client) Export(ctx context.Context, vm, snapshot string) (*Lease, error) {
	body := `<urn:ExportVm>` + Ref{"VirtualMachine", vm}.xml("_this") + `</urn:ExportVm>`
	tag := "ExportVmResponse"
	if snapshot != "" {
		body, tag = `<urn:ExportSnapshot>`+Ref{"VirtualMachineSnapshot", snapshot}.xml("_this")+`</urn:ExportSnapshot>`, "ExportSnapshotResponse"
	}
	var res struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := c.call(ctx, body, &res); err != nil {
		return nil, err
	}
	var lr struct {
		XMLName xml.Name
		R       Ref `xml:"returnval"`
	}
	if err := xml.Unmarshal(res.Body.Inner, &lr); err != nil || lr.XMLName.Local != tag {
		return nil, errors.New("unexpected export response")
	}
	l := &Lease{c: c, ref: lr.R}
	for {
		o, err := c.props(ctx, l.ref, "state", "error.localizedMessage")
		if err != nil {
			return nil, err
		}
		switch o.get("state") {
		case "ready":
			return l, l.load(ctx)
		case "error":
			return nil, fmt.Errorf("export failed: %s", o.get("error.localizedMessage"))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (l *Lease) load(ctx context.Context) error {
	o, err := l.c.props(ctx, l.ref, "info")
	if err != nil {
		return err
	}
	var info struct {
		DeviceURL []struct {
			URL        string `xml:"url"`
			Thumbprint string `xml:"sslThumbprint"`
			Disk       bool   `xml:"disk"`
		} `xml:"deviceUrl"`
	}
	if err := xml.Unmarshal([]byte("<i>"+o.get("info")+"</i>"), &info); err != nil {
		return err
	}
	for _, d := range info.DeviceURL {
		if !d.Disk {
			continue
		}
		u, err := url.Parse(d.URL)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "*" || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
			u.Host = l.c.host
		}
		l.Disks = append(l.Disks, DiskURL{u.String(), d.Thumbprint})
	}
	return nil
}

func (l *Lease) Progress(ctx context.Context, pct int) error {
	return l.c.call(ctx, `<urn:HttpNfcLeaseProgress>`+l.ref.xml("_this")+fmt.Sprintf(`<urn:percent>%d</urn:percent></urn:HttpNfcLeaseProgress>`, min(max(pct, 0), 100)), nil)
}

func (l *Lease) Complete(ctx context.Context) error {
	return l.c.call(ctx, `<urn:HttpNfcLeaseComplete>`+l.ref.xml("_this")+`</urn:HttpNfcLeaseComplete>`, nil)
}

func (l *Lease) Abort(ctx context.Context) error {
	return l.c.call(ctx, `<urn:HttpNfcLeaseAbort>`+l.ref.xml("_this")+`</urn:HttpNfcLeaseAbort>`, nil)
}

func (l *Lease) Open(ctx context.Context, d DiskURL, vcenterTLS *tls.Config) (*http.Response, error) {
	tc := vcenterTLS.Clone()
	if want := strings.ToLower(strings.ReplaceAll(d.Thumbprint, ":", "")); want != "" {
		tc.InsecureSkipVerify, tc.VerifyPeerCertificate = true, func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			sum := sha1.Sum(raw[0])
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("host certificate does not match vCenter's thumbprint")
			}
			return nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Jar: l.c.hc.Jar, Transport: &http.Transport{TLSClientConfig: tc, ResponseHeaderTimeout: 2 * time.Minute}}
	resp, err := hc.Do(req)
	if err == nil && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("disk download: %s", resp.Status)
	}
	return resp, err
}
