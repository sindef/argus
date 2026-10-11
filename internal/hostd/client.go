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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://hostd/backup/files/"+rel, body)
	if err != nil {
		return err
	}
	resp, err := c.raw.Do(req)
	if err != nil {
		return fmt.Errorf("host helper unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return errors.New(e.Error)
	}
	return nil
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
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
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
