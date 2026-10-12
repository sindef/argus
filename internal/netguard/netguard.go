package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

var clusterNets = []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("10.43.0.0/16")}

func Allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range clusterNets {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func CheckHost(ctx context.Context, host string) error {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if !Allowed(ip) {
			return fmt.Errorf("%s resolves to %s, which is not allowed", host, ip.Unmap())
		}
	}
	return nil
}

func CheckURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("URL must be http or https")
	}
	return CheckHost(ctx, u.Host)
}

func control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if !Allowed(ap.Addr()) {
		return fmt.Errorf("connections to %s are not allowed", ap.Addr().Unmap())
	}
	return nil
}

func Dialer() *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, Control: control}
}

func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: Dialer().DialContext, TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 2 * time.Minute}}
}
