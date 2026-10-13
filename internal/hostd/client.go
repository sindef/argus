package hostd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	hc     *http.Client
	raw    *http.Client
	nodeIP string
}

func NewClient(socket, nodeIP string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{nodeIP: nodeIP, hc: &http.Client{Timeout: 10 * time.Minute, Transport: tr}, raw: &http.Client{Transport: tr}}
}

func (c *Client) PutBackupFile(ctx context.Context, rel string, body io.Reader) error {
	resp, err := c.stream(ctx, http.MethodPut, "/backup/files/"+rel, body)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (c *Client) stream(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://hostd"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.raw.Do(req)
	if err != nil {
		return nil, fmt.Errorf("host helper unavailable: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct{ Error string }
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		return nil, errors.New(e.Error)
	}
	return resp, nil
}

func (c *Client) ZFSSnapshots(ctx context.Context, datasets []string, kind string) (map[string][]string, error) {
	var out map[string][]string
	return out, c.do(ctx, http.MethodGet, "/zfs/snapshots?"+url.Values{"ds": datasets, "kind": {kind}}.Encode(), nil, &out)
}

func (c *Client) ZFSChanges(ctx context.Context, ds, snap, from string) (Changes, error) {
	var out Changes
	return out, c.do(ctx, http.MethodGet, "/zfs/changes?"+url.Values{"ds": {ds}, "snap": {snap}, "from": {from}}.Encode(), nil, &out)
}

func (c *Client) ZFSRead(ctx context.Context, ds, snap string, h http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://hostd/zfs/read?"+url.Values{"ds": {ds}, "snap": {snap}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"Range", "If-Range"} {
		if v := h.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.raw.Do(req)
	if err != nil {
		return nil, fmt.Errorf("host helper unavailable: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		defer resp.Body.Close()
		var e struct{ Error string }
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		return nil, errors.New(e.Error)
	}
	return resp, nil
}

func (c *Client) FLRLoad(ctx context.Context, id, disk string, image io.Reader) error {
	resp, err := c.stream(ctx, http.MethodPut, "/flr/"+id+"/"+disk, image)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (c *Client) FLRAttachVolume(ctx context.Context, id, disk, dataset string) error {
	return c.do(ctx, http.MethodPost, "/flr/"+id+"/"+disk+"/attach", map[string]string{"dataset": dataset}, nil)
}

func (c *Client) FLRParts(ctx context.Context, id string) ([]FLRPart, error) {
	var out []FLRPart
	return out, c.do(ctx, http.MethodGet, "/flr/"+id, nil, &out)
}

func (c *Client) FLRList(ctx context.Context, id, part, path string) ([]FLREntry, error) {
	var out []FLREntry
	return out, c.do(ctx, http.MethodGet, "/flr/"+id+"/ls?"+url.Values{"part": {part}, "path": {path}}.Encode(), nil, &out)
}

func (c *Client) FLRFile(ctx context.Context, id, part, path string) (*http.Response, error) {
	return c.stream(ctx, http.MethodGet, "/flr/"+id+"/file?"+url.Values{"part": {part}, "path": {path}}.Encode(), nil)
}

func (c *Client) FLRClose(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/flr/"+id+"/close", nil, nil)
}

func (c *Client) FLRSweep(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/flr/sweep", nil, nil)
}

func (c *Client) ZVols(ctx context.Context, datasets []string) (map[string]ZVol, error) {
	var out map[string]ZVol
	return out, c.do(ctx, http.MethodGet, "/zfs/volumes?"+url.Values{"ds": datasets}.Encode(), nil, &out)
}

func (c *Client) SetThick(ctx context.Context, dataset string, thick bool) error {
	return c.do(ctx, http.MethodPost, "/zfs/provisioning", map[string]any{"dataset": dataset, "thick": thick}, nil)
}

func (c *Client) ReleaseVolume(ctx context.Context, dataset string) error {
	return c.do(ctx, http.MethodPost, "/zfs/release", map[string]string{"dataset": dataset}, nil)
}

func (c *Client) SweepSnapshots(ctx context.Context, keep []string) ([]string, error) {
	var gone []string
	return gone, c.do(ctx, http.MethodPost, "/zfs/sweep", map[string]any{"keep": keep}, &gone)
}

func (c *Client) ZFSDestroy(ctx context.Context, datasets []string, snap string) error {
	return c.do(ctx, http.MethodPost, "/zfs/destroy", map[string]any{"datasets": datasets, "snap": snap}, nil)
}

func (c *Client) ZFSSnapshot(ctx context.Context, datasets []string, snap string) error {
	return c.do(ctx, http.MethodPost, "/zfs/snapshot", map[string]any{"datasets": datasets, "snap": snap}, nil)
}

func (c *Client) ZFSPrune(ctx context.Context, datasets, keep []string, kind string) error {
	return c.do(ctx, http.MethodPost, "/zfs/prune", map[string]any{"datasets": datasets, "keep": keep, "kind": kind}, nil)
}

func (c *Client) ZFSSend(ctx context.Context, ds, snap, from string) (*http.Response, error) {
	return c.stream(ctx, http.MethodGet, "/zfs/send?"+url.Values{"ds": {ds}, "snap": {snap}, "from": {from}}.Encode(), nil)
}

func (c *Client) ZFSRecv(ctx context.Context, ds string, full bool, body io.Reader) error {
	q := url.Values{"ds": {ds}}
	if full {
		q.Set("full", "1")
	}
	resp, err := c.stream(ctx, http.MethodPut, "/zfs/recv?"+q.Encode(), body)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (c *Client) GetBackupFile(ctx context.Context, rel string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://hostd/backup/files/"+rel, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.raw.Do(req)
	if err == nil && resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("backup file %s: %s", rel, resp.Status)
	}
	return resp, err
}

func (c *Client) Backups(ctx context.Context) ([]Backup, error) {
	var bs []Backup
	return bs, c.do(ctx, http.MethodGet, "/backup/list", nil, &bs)
}

func (c *Client) DeleteBackup(ctx context.Context, rel string) error {
	return c.do(ctx, http.MethodPost, "/backup/delete", map[string]string{"path": rel}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hostd"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(NodeIPHeader, c.nodeIP)
	if a, ok := ctx.Value(accessKey{}).(string); ok {
		req.Header.Set(AccessHeader, a)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("host helper unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Disks(ctx context.Context) ([]Disk, error) {
	var ds []Disk
	return ds, c.do(ctx, http.MethodGet, "/disks", nil, &ds)
}

func (c *Client) Disk(ctx context.Context, name string) (DiskDetail, error) {
	var dd DiskDetail
	return dd, c.do(ctx, http.MethodGet, "/disks/"+name, nil, &dd)
}

func (c *Client) DiskAction(ctx context.Context, name, action, confirm, test string) error {
	return c.do(ctx, http.MethodPost, "/disks/"+name+"/"+action, map[string]string{"confirm": confirm, "test": test}, nil)
}

func (c *Client) Pools(ctx context.Context) (Pools, error) {
	var ps Pools
	return ps, c.do(ctx, http.MethodGet, "/pools", nil, &ps)
}

func (c *Client) CreatePool(ctx context.Context, spec PoolSpec) error {
	return c.do(ctx, http.MethodPost, "/pools", spec, nil)
}

func (c *Client) AddToPool(ctx context.Context, name string, spec PoolSpec) error {
	return c.do(ctx, http.MethodPost, "/pools/"+name+"/add", spec, nil)
}

func (c *Client) ImportPool(ctx context.Context, name string, force bool) error {
	path := "/pools/" + name + "/import"
	if force {
		path += "?force=1"
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

func (c *Client) ReleasePool(ctx context.Context, name, action, confirm string) error {
	return c.do(ctx, http.MethodPost, "/pools/"+name+"/"+action, map[string]string{"confirm": confirm}, nil)
}

func (c *Client) Network(ctx context.Context) (HostNet, error) {
	var hn HostNet
	return hn, c.do(ctx, http.MethodGet, "/network", nil, &hn)
}

func (c *Client) CreateBridge(ctx context.Context, spec BridgeSpec) error {
	return c.do(ctx, http.MethodPost, "/network/bridges", spec, nil)
}

func (c *Client) DeleteBridge(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/network/bridges/"+name+"/delete", nil, nil)
}

func (c *Client) SetTeaming(ctx context.Context, name string, t Teaming) error {
	return c.do(ctx, http.MethodPost, "/network/bridges/"+name+"/teaming", t, nil)
}

func (c *Client) BridgePorts(ctx context.Context, name string) ([]BridgePort, error) {
	var out []BridgePort
	return out, c.do(ctx, http.MethodGet, "/network/bridges/"+name+"/ports", nil, &out)
}

func (c *Client) Devices(ctx context.Context) (Devices, error) {
	var out Devices
	return out, c.do(ctx, http.MethodGet, "/devices", nil, &out)
}

func (c *Client) SetPassthrough(ctx context.Context, kind, id string, on bool) error {
	return c.do(ctx, http.MethodPost, "/devices", map[string]any{"kind": kind, "id": id, "on": on}, nil)
}

func (c *Client) SetAddress(ctx context.Context, a AddressSpec) error {
	return c.do(ctx, http.MethodPost, "/network/address", a, nil)
}

func (c *Client) SetManagement(ctx context.Context, iface string, on bool) error {
	return c.do(ctx, http.MethodPost, "/network/management", map[string]any{"iface": iface, "on": on}, nil)
}

func (c *Client) NetworkDecision(ctx context.Context, op string) error {
	return c.do(ctx, http.MethodPost, "/network/"+op, nil, nil)
}

func (c *Client) Settings(ctx context.Context) (Settings, error) {
	var st Settings
	return st, c.do(ctx, http.MethodGet, "/settings", nil, &st)
}

func (c *Client) ApplySettings(ctx context.Context, u SettingsUpdate) error {
	return c.do(ctx, http.MethodPost, "/settings", u, nil)
}

func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	return st, c.do(ctx, http.MethodGet, "/stats", nil, &st)
}

type accessKey struct{}

func WithAccess(ctx context.Context, host string) context.Context {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return context.WithValue(ctx, accessKey{}, host)
}

func (c *Client) Firewall(ctx context.Context, vms []FWVM) error {
	return c.do(ctx, http.MethodPost, "/firewall", vms, nil)
}
