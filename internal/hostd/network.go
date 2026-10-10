package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	networkDir   = "/etc/systemd/network"
	managedGlob  = "01-argus-*"
	snapshotDir  = "/run/argus-hostd/rollback"
	PendingFile  = "/run/argus/net-pending.json"
	rollbackUnit = "argus-net-rollback"
	rollbackSecs = 120
)

var (
	validIface = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)
	bondModes  = []string{"active-backup", "802.3ad", "balance-alb", "balance-tlb", "balance-xor", "balance-rr", "broadcast"}
)

type Iface struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	MAC       string   `json:"mac"`
	State     string   `json:"state"`
	Speed     int      `json:"speed"`
	Master    string   `json:"master"`
	Ports     []string `json:"ports"`
	Addresses []string `json:"addresses"`
	Managed   bool     `json:"managed"`
	Mgmt      bool     `json:"mgmt"`
}

type HostNet struct {
	Ifaces  []Iface  `json:"ifaces"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
	Pending *Pending `json:"pending"`
}

type Pending struct {
	Description string    `json:"description"`
	Deadline    time.Time `json:"deadline"`
	Created     []string  `json:"created"`
}

type BridgeSpec struct {
	Name     string   `json:"name"`
	Ports    []string `json:"ports"`
	Bond     string   `json:"bond"`
	BondMode string   `json:"bondMode"`
}

type AddressSpec struct {
	Iface   string   `json:"iface"`
	DHCP    bool     `json:"dhcp"`
	Address string   `json:"address"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

type ipLink struct {
	Ifname    string `json:"ifname"`
	Address   string `json:"address"`
	Operstate string `json:"operstate"`
	Master    string `json:"master"`
	Linkinfo  struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func physical(name string) bool {
	_, err := os.Stat(filepath.Join("/sys/class/net", name, "device"))
	return err == nil
}

func readPending() *Pending {
	b, err := os.ReadFile(PendingFile)
	if err != nil {
		return nil
	}
	var p Pending
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func hostNet(ctx context.Context) (HostNet, error) {
	var hn HostNet
	out, err := run(ctx, "ip", "-j", "-d", "addr")
	if err != nil {
		return hn, err
	}
	var links []ipLink
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return hn, err
	}
	routes, _ := run(ctx, "ip", "-j", "route", "show", "default")
	var defs []struct{ Gateway, Dev string }
	json.Unmarshal([]byte(routes), &defs)
	mgmt := ""
	if len(defs) > 0 {
		hn.Gateway, mgmt = defs[0].Gateway, defs[0].Dev
	}
	ips, _ := ctx.Value(nodeIPKey{}).([]string)
	protected := map[string]bool{}
	for i, ip := range ips {
		for _, l := range links {
			for _, a := range l.AddrInfo {
				if ip != "" && a.Local == ip {
					protected[l.Ifname] = true
					if i == 0 {
						mgmt = l.Ifname
					}
				}
			}
		}
	}
	if len(protected) == 0 && mgmt != "" {
		protected[mgmt] = true
	}
	for _, d := range defs {
		if d.Dev == mgmt {
			hn.Gateway = d.Gateway
		}
	}
	managed := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(networkDir, managedGlob))
	for _, f := range files {
		base := strings.TrimPrefix(filepath.Base(f), "01-argus-")
		managed[strings.TrimSuffix(base, filepath.Ext(base))] = true
	}
	byName := map[string]*Iface{}
	for _, l := range links {
		kind := l.Linkinfo.InfoKind
		switch {
		case kind == "bridge" && (l.Ifname == "cni0" || strings.HasPrefix(l.Ifname, "docker")):
			continue
		case kind == "bridge" || kind == "bond" || kind == "vlan":
		case kind == "" && physical(l.Ifname):
			kind = "ether"
		default:
			continue
		}
		i := &Iface{Name: l.Ifname, Kind: kind, MAC: l.Address, State: strings.ToLower(l.Operstate), Master: l.Master, Managed: managed[l.Ifname], Mgmt: protected[l.Ifname]}
		if b, err := os.ReadFile(filepath.Join("/sys/class/net", l.Ifname, "speed")); err == nil {
			fmt.Sscanf(string(b), "%d", &i.Speed)
		}
		for _, a := range l.AddrInfo {
			if a.Family == "inet" || (a.Family == "inet6" && !strings.HasPrefix(a.Local, "fe80")) {
				i.Addresses = append(i.Addresses, fmt.Sprintf("%s/%d", a.Local, a.Prefixlen))
			}
		}
		byName[l.Ifname] = i
	}
	for _, i := range byName {
		if m, ok := byName[i.Master]; ok {
			m.Ports = append(m.Ports, i.Name)
			if i.Mgmt || m.Mgmt {
				m.Mgmt = true
			}
		}
	}
	for _, i := range byName {
		sort.Strings(i.Ports)
		hn.Ifaces = append(hn.Ifaces, *i)
	}
	sort.Slice(hn.Ifaces, func(a, b int) bool { return hn.Ifaces[a].Name < hn.Ifaces[b].Name })
	if b, err := os.ReadFile("/run/systemd/resolve/resolv.conf"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "nameserver "); ok {
				hn.DNS = append(hn.DNS, strings.TrimSpace(v))
			}
		}
	}
	hn.Pending = readPending()
	return hn, nil
}

func unit(name, ext, body string) (string, string) {
	return filepath.Join(networkDir, "01-argus-"+name+"."+ext), body
}

func portNetwork(port, master, kind string) (string, string) {
	body := fmt.Sprintf("[Match]\nName=%s\n\n[Network]\n%s=%s\n", port, kind, master)
	if kind == "Bridge" {
		body += "\n[BridgeVLAN]\nVLAN=2-4094\n"
	}
	return unit(port, "network", body)
}

func (s *Server) createBridge(ctx context.Context, spec BridgeSpec) error {
	hn, err := hostNet(ctx)
	if err != nil {
		return err
	}
	exists := map[string]Iface{}
	for _, i := range hn.Ifaces {
		exists[i.Name] = i
	}
	if !validIface.MatchString(spec.Name) {
		return errors.New("bridge name must be up to 15 lowercase letters, digits or dashes")
	}
	if _, ok := exists[spec.Name]; ok {
		return fmt.Errorf("%s already exists", spec.Name)
	}
	if len(spec.Ports) == 0 {
		return errors.New("select at least one network port")
	}
	taken := map[string]int{}
	for _, p := range spec.Ports {
		i, ok := exists[p]
		switch {
		case !ok || i.Kind != "ether":
			return fmt.Errorf("%s is not a physical network port", p)
		case i.Mgmt:
			return fmt.Errorf("%s carries the management address; move the address first", p)
		}
		if i.Master != "" {
			taken[i.Master]++
		}
	}
	for master, n := range taken {
		if m := exists[master]; m.Mgmt && len(m.Ports) <= n {
			return fmt.Errorf("%s needs at least one port left for the management connection", master)
		}
	}
	files := map[string]string{}
	add := func(path, body string) { files[path] = body }
	add(unit(spec.Name, "netdev", fmt.Sprintf("[NetDev]\nName=%s\nKind=bridge\n\n[Bridge]\nVLANFiltering=yes\nDefaultPVID=1\n", spec.Name)))
	add(unit(spec.Name, "network", fmt.Sprintf("[Match]\nName=%s\n\n[Network]\nLinkLocalAddressing=no\nConfigureWithoutCarrier=yes\n", spec.Name)))
	created := []string{spec.Name}
	if spec.Bond != "" {
		if !validIface.MatchString(spec.Bond) || exists[spec.Bond].Name != "" {
			return fmt.Errorf("invalid or existing bond name %q", spec.Bond)
		}
		if len(spec.Ports) < 2 || !slices.Contains(bondModes, spec.BondMode) {
			return errors.New("a bond needs at least two ports and a valid mode")
		}
		add(unit(spec.Bond, "netdev", fmt.Sprintf("[NetDev]\nName=%s\nKind=bond\n\n[Bond]\nMode=%s\nMIIMonitorSec=100ms\n", spec.Bond, spec.BondMode)))
		add(portNetwork(spec.Bond, spec.Name, "Bridge"))
		for _, p := range spec.Ports {
			add(portNetwork(p, spec.Bond, "Bond"))
		}
		created = append(created, spec.Bond)
	} else {
		for _, p := range spec.Ports {
			add(portNetwork(p, spec.Name, "Bridge"))
		}
	}
	return s.apply(ctx, "create bridge "+spec.Name, files, nil, created, slices.Concat(spec.Ports, created))
}

func (s *Server) deleteBridge(ctx context.Context, name string) error {
	hn, err := hostNet(ctx)
	if err != nil {
		return err
	}
	var br *Iface
	for i := range hn.Ifaces {
		if hn.Ifaces[i].Name == name {
			br = &hn.Ifaces[i]
		}
	}
	switch {
	case br == nil || br.Kind != "bridge":
		return fmt.Errorf("bridge %s not found", name)
	case !br.Managed:
		return fmt.Errorf("%s comes from the install configuration and cannot be removed here", name)
	case br.Mgmt:
		return fmt.Errorf("%s carries the management connection", name)
	}
	var remove []string
	affected := []string{name}
	members := br.Ports
	for _, p := range br.Ports {
		for _, i := range hn.Ifaces {
			if i.Name == p && i.Kind == "bond" && i.Managed {
				members = append(members, i.Ports...)
				remove = append(remove, filepath.Join(networkDir, "01-argus-"+p+".netdev"), filepath.Join(networkDir, "01-argus-"+p+".network"))
				affected = append(affected, p)
			}
		}
	}
	remove = append(remove, filepath.Join(networkDir, "01-argus-"+name+".netdev"), filepath.Join(networkDir, "01-argus-"+name+".network"))
	standalone := map[string]string{}
	for _, m := range members {
		if slices.Contains(affected, m) {
			continue
		}
		path, body := unit(m, "network", fmt.Sprintf("[Match]\nName=%s\n\n[Network]\nLinkLocalAddressing=no\n", m))
		standalone[path] = body
	}
	if err := s.apply(ctx, "delete bridge "+name, standalone, remove, nil, slices.Concat(members, affected)); err != nil {
		return err
	}
	for _, a := range affected {
		run(ctx, "ip", "link", "del", a)
	}
	return nil
}

func (s *Server) setAddress(ctx context.Context, a AddressSpec) error {
	hn, err := hostNet(ctx)
	if err != nil {
		return err
	}
	var target *Iface
	for i := range hn.Ifaces {
		if hn.Ifaces[i].Name == a.Iface {
			target = &hn.Ifaces[i]
		}
	}
	if target == nil {
		return fmt.Errorf("interface %s not found", a.Iface)
	}
	if target.Master != "" {
		return fmt.Errorf("%s is a port of %s; set the address on %s", a.Iface, target.Master, target.Master)
	}
	body := fmt.Sprintf("[Match]\nName=%s\n\n[Network]\n", a.Iface)
	if a.DHCP {
		body += "DHCP=yes\n"
	} else {
		ip, _, err := net.ParseCIDR(a.Address)
		if err != nil || ip.To4() == nil {
			return errors.New("address must be IPv4 CIDR, e.g. 192.168.1.10/24")
		}
		body += "Address=" + a.Address + "\n"
		if a.Gateway != "" {
			if net.ParseIP(a.Gateway) == nil {
				return errors.New("invalid gateway")
			}
			body += "Gateway=" + a.Gateway + "\n"
		}
	}
	for _, d := range a.DNS {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("invalid DNS server %q", d)
		}
		body += "DNS=" + d + "\n"
	}
	if target.Kind == "bridge" {
		body += "ConfigureWithoutCarrier=yes\n"
	}
	if a.DHCP {
		body += "\n[DHCPv4]\nClientIdentifier=mac\n"
	}
	path, _ := unit(a.Iface, "network", "")
	return s.apply(ctx, "set address of "+a.Iface, map[string]string{path: body}, nil, nil, []string{a.Iface})
}

func (s *Server) apply(ctx context.Context, desc string, write map[string]string, remove, created, affected []string) error {
	if readPending() != nil {
		return errors.New("a network change is waiting for confirmation; confirm or roll it back first")
	}
	os.RemoveAll(snapshotDir)
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return err
	}
	current, _ := filepath.Glob(filepath.Join(networkDir, managedGlob))
	for _, f := range current {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(snapshotDir, filepath.Base(f)), b, 0o644); err != nil {
			return err
		}
	}
	p := Pending{Description: desc, Deadline: time.Now().Add(rollbackSecs * time.Second), Created: created}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(PendingFile, b, 0o644); err != nil {
		return err
	}
	if _, err := run(ctx, "systemd-run", "--collect", "--unit="+rollbackUnit, fmt.Sprintf("--on-active=%d", rollbackSecs), "/usr/sbin/argus-hostd", "-rollback"); err != nil {
		os.Remove(PendingFile)
		return err
	}
	for path, body := range write {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			Rollback(ctx)
			return err
		}
	}
	for _, path := range remove {
		os.Remove(path)
	}
	return reloadNetwork(ctx, affected)
}

func reloadNetwork(ctx context.Context, ifaces []string) error {
	if _, err := run(ctx, "networkctl", "reload"); err != nil {
		return err
	}
	for _, i := range ifaces {
		run(ctx, "networkctl", "reconfigure", i)
	}
	return nil
}

func Confirm(ctx context.Context) error {
	if readPending() == nil {
		return errors.New("nothing to confirm")
	}
	run(ctx, "systemctl", "stop", rollbackUnit+".timer")
	os.RemoveAll(snapshotDir)
	return os.Remove(PendingFile)
}

func Rollback(ctx context.Context) error {
	p := readPending()
	if p == nil {
		return errors.New("nothing to roll back")
	}
	run(ctx, "systemctl", "stop", rollbackUnit+".timer")
	current, _ := filepath.Glob(filepath.Join(networkDir, managedGlob))
	for _, f := range current {
		os.Remove(f)
	}
	saved, _ := filepath.Glob(filepath.Join(snapshotDir, "*"))
	var ifaces []string
	for _, f := range saved {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		os.WriteFile(filepath.Join(networkDir, filepath.Base(f)), b, 0o644)
	}
	for _, f := range append(current, saved...) {
		base := strings.TrimPrefix(filepath.Base(f), "01-argus-")
		ifaces = append(ifaces, strings.TrimSuffix(base, filepath.Ext(base)))
	}
	for _, c := range p.Created {
		run(ctx, "ip", "link", "del", c)
	}
	reloadNetwork(ctx, ifaces)
	os.RemoveAll(snapshotDir)
	return os.Remove(PendingFile)
}
