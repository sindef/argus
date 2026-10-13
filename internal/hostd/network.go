package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	mgmtFile     = "/usr/local/argus/management"
	firewallFile = "/run/argus-hostd/firewall.nft"
)

var mgmtPorts = "22, 443, 6443, 10250"

var (
	validIface = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)
	bondModes  = []string{"active-backup", "802.3ad", "balance-alb", "balance-tlb", "balance-xor", "balance-rr", "broadcast"}
	hashModes  = []string{"802.3ad", "balance-xor", "balance-tlb", "balance-alb"}
	hashes     = []string{"layer2", "layer2+3", "layer3+4", "encap2+3", "encap3+4"}
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
	Access    bool     `json:"access"`
	V4        string   `json:"v4"`
	V6        string   `json:"v6"`
	Static4   string   `json:"static4"`
	Static6   string   `json:"static6"`
	Gateway4  string   `json:"gateway4"`
	Gateway6  string   `json:"gateway6"`
	MTU       int      `json:"mtu"`
	BondMode  string   `json:"bondMode,omitempty"`
	Hash      string   `json:"hash,omitempty"`
	Primary   string   `json:"primary,omitempty"`
	Active    string   `json:"active,omitempty"`
}

type HostNet struct {
	Ifaces     []Iface  `json:"ifaces"`
	Gateway    string   `json:"gateway"`
	DNS        []string `json:"dns"`
	Pending    *Pending `json:"pending"`
	Restricted bool     `json:"restricted"`
	Management []string `json:"management"`
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
	Hash     string   `json:"hash"`
	MTU      int      `json:"mtu"`
}

type Teaming struct {
	Mode    string `json:"mode"`
	Hash    string `json:"hash"`
	Primary string `json:"primary"`
	MTU     int    `json:"mtu"`
}

type BridgePort struct {
	Name      string   `json:"name"`
	Uplink    bool     `json:"uplink"`
	State     string   `json:"state"`
	MACs      []string `json:"macs"`
	VLAN      int      `json:"vlan"`
	RxBytes   int64    `json:"rxBytes"`
	TxBytes   int64    `json:"txBytes"`
	RxDropped int64    `json:"rxDropped"`
	TxDropped int64    `json:"txDropped"`
}

type AddressSpec struct {
	Iface    string   `json:"iface"`
	IPv4     string   `json:"ipv4"`
	Address  string   `json:"address"`
	Gateway  string   `json:"gateway"`
	IPv6     string   `json:"ipv6"`
	Address6 string   `json:"address6"`
	Gateway6 string   `json:"gateway6"`
	DNS      []string `json:"dns"`
}

type ipLink struct {
	Ifindex   int    `json:"ifindex"`
	LinkIndex int    `json:"link_index"`
	NetnsID   *int   `json:"link_netnsid"`
	Ifname    string `json:"ifname"`
	Address   string `json:"address"`
	Operstate string `json:"operstate"`
	Master    string `json:"master"`
	Mtu       int    `json:"mtu"`
	Linkinfo  struct {
		InfoKind string `json:"info_kind"`
		InfoData struct {
			Mode           string `json:"mode"`
			XmitHashPolicy string `json:"xmit_hash_policy"`
			Primary        string `json:"primary"`
			ActiveSlave    string `json:"active_slave"`
		} `json:"info_data"`
	} `json:"linkinfo"`
	Stats64 struct {
		Rx, Tx struct{ Bytes, Dropped int64 }
	} `json:"stats64"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
		Dynamic   bool   `json:"dynamic"`
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
	defs, err := defaultRoutes(ctx, "-4")
	if err != nil {
		return hn, err
	}
	defs6, err := defaultRoutes(ctx, "-6")
	if err != nil {
		return hn, err
	}
	mgmt := ""
	if len(defs) > 0 {
		hn.Gateway, mgmt = defs[0].Gateway, defs[0].Dev
	}
	ips, _ := ctx.Value(nodeIPKey{}).([]string)
	protected, access := map[string]bool{}, map[string]bool{}
	for i, ip := range ips {
		for _, l := range links {
			for _, a := range l.AddrInfo {
				if ip != "" && a.Local == ip {
					protected[l.Ifname] = true
					if i == 0 {
						mgmt = l.Ifname
					} else {
						access[l.Ifname] = true
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
		i := &Iface{Name: l.Ifname, Kind: kind, MAC: l.Address, State: strings.ToLower(l.Operstate), Master: l.Master, Managed: managed[l.Ifname], Mgmt: protected[l.Ifname], Access: access[l.Ifname], V4: "off", V6: "off", MTU: l.Mtu}
		if kind == "bond" {
			d := l.Linkinfo.InfoData
			i.BondMode, i.Primary, i.Active = d.Mode, d.Primary, d.ActiveSlave
			if slices.Contains(hashModes, d.Mode) {
				i.Hash = d.XmitHashPolicy
			}
		}
		if b, err := os.ReadFile(filepath.Join("/sys/class/net", l.Ifname, "speed")); err == nil {
			i.Speed, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		for _, a := range l.AddrInfo {
			cidr := fmt.Sprintf("%s/%d", a.Local, a.Prefixlen)
			switch {
			case a.Family == "inet" && a.Dynamic:
				i.V4 = "dhcp"
			case a.Family == "inet":
				i.V4, i.Static4 = "static", cidr
			case strings.HasPrefix(a.Local, "fe80"):
				continue
			case a.Dynamic:
				i.V6 = "auto"
			default:
				i.V6, i.Static6 = "static", cidr
			}
			i.Addresses = append(i.Addresses, cidr)
		}
		for _, d := range defs {
			if d.Dev == l.Ifname {
				i.Gateway4 = d.Gateway
			}
		}
		for _, d := range defs6 {
			if d.Dev == l.Ifname {
				i.Gateway6 = d.Gateway
			}
		}
		byName[l.Ifname] = i
	}
	for _, i := range byName {
		if m, ok := byName[i.Master]; ok {
			m.Ports = append(m.Ports, i.Name)
			m.Mgmt = m.Mgmt || i.Mgmt
			m.Access = m.Access || i.Access
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
	hn.Management, hn.Restricted = readMgmt()
	return hn, nil
}

func unit(name, ext, body string) (string, string) {
	return filepath.Join(networkDir, "01-argus-"+name+"."+ext), body
}

func mtuLink(mtu int) string {
	if mtu == 0 {
		return ""
	}
	return fmt.Sprintf("\n[Link]\nMTUBytes=%d\n", mtu)
}

func portNetwork(port, master, kind string, mtu int, primary bool) (string, string) {
	body := fmt.Sprintf("[Match]\nName=%s\n\n[Network]\n%s=%s\n", port, kind, master)
	if primary {
		body += "PrimarySlave=yes\n"
	}
	if kind == "Bridge" {
		body += "\n[BridgeVLAN]\nVLAN=2-4094\n"
	}
	return unit(port, "network", body+mtuLink(mtu))
}

func bridgeNetwork(name string, mtu int) (string, string) {
	return unit(name, "network", fmt.Sprintf("[Match]\nName=%s\n\n[Network]\nLinkLocalAddressing=no\nConfigureWithoutCarrier=yes\n", name)+mtuLink(mtu))
}

func bondNetdev(name string, t Teaming) (string, string) {
	body := fmt.Sprintf("[NetDev]\nName=%s\nKind=bond\n\n[Bond]\nMode=%s\nMIIMonitorSec=100ms\n", name, t.Mode)
	if t.Mode == "802.3ad" {
		body += "LACPTransmitRate=fast\n"
	}
	if t.Hash != "" && slices.Contains(hashModes, t.Mode) {
		body += "TransmitHashPolicy=" + t.Hash + "\n"
	}
	return unit(name, "netdev", body)
}

func (t Teaming) validate(bonded bool) error {
	switch {
	case t.MTU != 0 && (t.MTU < 1280 || t.MTU > 9216):
		return errors.New("MTU must be 1280-9216")
	case !bonded && (t.Hash != "" || t.Primary != ""):
		return errors.New("teaming needs at least two uplinks")
	case bonded && !slices.Contains(bondModes, t.Mode):
		return errors.New("invalid teaming mode")
	case t.Hash != "" && !slices.Contains(hashes, t.Hash):
		return errors.New("invalid hash policy")
	case t.Primary != "" && t.Mode != "active-backup":
		return errors.New("a primary uplink applies to active-backup only")
	}
	return nil
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
	if err := (Teaming{Mode: spec.BondMode, Hash: spec.Hash, MTU: spec.MTU}).validate(spec.Bond != ""); err != nil {
		return err
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
	add(unit(spec.Name, "netdev", fmt.Sprintf("[NetDev]\nName=%s\nKind=bridge\nMACAddress=%s\n\n[Bridge]\nVLANFiltering=yes\nDefaultPVID=1\n", spec.Name, exists[spec.Ports[0]].MAC)))
	add(bridgeNetwork(spec.Name, spec.MTU))
	created := []string{spec.Name}
	if spec.Bond != "" {
		if !validIface.MatchString(spec.Bond) || exists[spec.Bond].Name != "" {
			return fmt.Errorf("invalid or existing bond name %q", spec.Bond)
		}
		if len(spec.Ports) < 2 || !slices.Contains(bondModes, spec.BondMode) {
			return errors.New("a bond needs at least two ports and a valid mode")
		}
		add(bondNetdev(spec.Bond, Teaming{Mode: spec.BondMode, Hash: spec.Hash}))
		add(portNetwork(spec.Bond, spec.Name, "Bridge", spec.MTU, false))
		for _, p := range spec.Ports {
			add(portNetwork(p, spec.Bond, "Bond", spec.MTU, false))
		}
		created = append(created, spec.Bond)
	} else {
		for _, p := range spec.Ports {
			add(portNetwork(p, spec.Name, "Bridge", spec.MTU, false))
		}
	}
	return s.apply(ctx, "create bridge "+spec.Name, files, nil, created, slices.Concat(spec.Ports, created))
}

func (s *Server) setTeaming(ctx context.Context, name string, t Teaming) error {
	hn, err := hostNet(ctx)
	if err != nil {
		return err
	}
	byName := map[string]Iface{}
	for _, i := range hn.Ifaces {
		byName[i.Name] = i
	}
	br, ok := byName[name]
	switch {
	case !ok || br.Kind != "bridge":
		return fmt.Errorf("bridge %s not found", name)
	case !br.Managed:
		return fmt.Errorf("%s comes from the install configuration and cannot be changed here", name)
	}
	var bond *Iface
	var uplinks []string
	for _, p := range br.Ports {
		if i := byName[p]; i.Kind == "bond" && i.Managed {
			bond, uplinks = &i, i.Ports
		} else if i.Kind == "ether" {
			uplinks = append(uplinks, p)
		}
	}
	if err := t.validate(bond != nil); err != nil {
		return err
	}
	if t.Primary != "" && !slices.Contains(uplinks, t.Primary) {
		return fmt.Errorf("%s is not an uplink of %s", t.Primary, name)
	}
	files := map[string]string{}
	add := func(path, body string) { files[path] = body }
	add(bridgeNetwork(name, t.MTU))
	affected := slices.Concat([]string{name}, uplinks)
	var recreate []string
	if bond != nil {
		add(bondNetdev(bond.Name, t))
		add(portNetwork(bond.Name, name, "Bridge", t.MTU, false))
		for _, p := range uplinks {
			add(portNetwork(p, bond.Name, "Bond", t.MTU, p == t.Primary))
		}
		affected = append(affected, bond.Name)
		if bond.BondMode != t.Mode || bond.Hash != t.Hash || bond.Primary != t.Primary {
			recreate = []string{bond.Name}
		}
	} else {
		for _, p := range uplinks {
			add(portNetwork(p, name, "Bridge", t.MTU, false))
		}
	}
	return s.apply(ctx, "set teaming of "+name, files, nil, recreate, affected)
}

func BridgePorts(ctx context.Context, name string) ([]BridgePort, error) {
	if !validIface.MatchString(name) {
		return nil, errors.New("invalid bridge name")
	}
	out, err := run(ctx, "ip", "-j", "-d", "-s", "link", "show", "master", name)
	if err != nil {
		return nil, err
	}
	var links []ipLink
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, err
	}
	var fdb []struct{ MAC, Ifname, State string }
	if out, err := run(ctx, "bridge", "-j", "fdb", "show", "br", name); err == nil {
		if err := json.Unmarshal([]byte(out), &fdb); err != nil {
			return nil, fmt.Errorf("bridge fdb: %w", err)
		}
	}
	var vlans []struct {
		Ifname string
		Vlans  []struct {
			Vlan  int
			Flags []string
		}
	}
	if out, err := run(ctx, "bridge", "-j", "vlan", "show"); err == nil {
		if err := json.Unmarshal([]byte(out), &vlans); err != nil {
			return nil, fmt.Errorf("bridge vlan: %w", err)
		}
	}
	netns := map[int]string{}
	if out, err := run(ctx, "ip", "-j", "netns", "list-id"); err == nil {
		var ids []struct {
			NSID int
			Name string
		}
		if err := json.Unmarshal([]byte(out), &ids); err != nil {
			return nil, fmt.Errorf("netns list: %w", err)
		}
		for _, id := range ids {
			netns[id.NSID] = id.Name
		}
	}
	inNS := map[string][]ipLink{}
	guestMAC := func(l ipLink) string {
		if l.NetnsID == nil || netns[*l.NetnsID] == "" {
			return ""
		}
		ns := netns[*l.NetnsID]
		if _, ok := inNS[ns]; !ok {
			var ls []ipLink
			if out, err := run(ctx, "ip", "-n", ns, "-j", "link", "show"); err == nil && json.Unmarshal([]byte(out), &ls) != nil {
				ls = nil
			}
			inNS[ns] = ls
		}
		for _, peer := range inNS[ns] {
			if hash, ok := strings.CutSuffix(peer.Ifname, "-nic"); ok && peer.Ifindex == l.LinkIndex {
				for _, d := range inNS[ns] {
					if d.Ifname == "pod"+hash {
						return d.Address
					}
				}
			}
		}
		return ""
	}
	var ports []BridgePort
	for _, l := range links {
		p := BridgePort{Name: l.Ifname, Uplink: l.Linkinfo.InfoKind == "bond" || physical(l.Ifname), State: strings.ToLower(l.Operstate),
			RxBytes: l.Stats64.Rx.Bytes, TxBytes: l.Stats64.Tx.Bytes, RxDropped: l.Stats64.Rx.Dropped, TxDropped: l.Stats64.Tx.Dropped}
		if m := guestMAC(l); m != "" {
			p.MACs = append(p.MACs, m)
		}
		for _, f := range fdb {
			if f.Ifname == l.Ifname && f.State != "permanent" && !slices.Contains(p.MACs, f.MAC) {
				p.MACs = append(p.MACs, f.MAC)
			}
		}
		for _, v := range vlans {
			if v.Ifname != l.Ifname {
				continue
			}
			for _, x := range v.Vlans {
				if slices.Contains(x.Flags, "PVID") {
					p.VLAN = x.Vlan
				}
			}
		}
		ports = append(ports, p)
	}
	return ports, nil
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
		if _, err := run(ctx, "ip", "link", "del", a); err != nil {
			log.Printf("delete bridge %s: %v", name, err)
		}
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
	if target.Access && a.IPv4 == "off" && a.IPv6 == "off" {
		return fmt.Errorf("you are connected through %s; it needs an address", a.Iface)
	}
	body, err := addressNetwork(a, target.Kind == "bridge")
	if err != nil {
		return err
	}
	path, _ := unit(a.Iface, "network", "")
	return s.apply(ctx, "set address of "+a.Iface, map[string]string{path: body}, nil, nil, []string{a.Iface})
}

func addressNetwork(a AddressSpec, bridge bool) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "[Match]\nName=%s\n\n[Network]\n", a.Iface)
	dhcp := map[[2]bool]string{{true, true}: "yes", {true, false}: "ipv4", {false, true}: "ipv6"}[[2]bool{a.IPv4 == "dhcp", a.IPv6 == "dhcp"}]
	if dhcp != "" {
		b.WriteString("DHCP=" + dhcp + "\n")
	}
	static := func(cidr, gw string, v4 bool, label string) error {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil || (ip.To4() != nil) != v4 {
			return fmt.Errorf("%s address must be a CIDR, e.g. %s", label, map[bool]string{true: "192.168.1.10/24", false: "2001:db8::10/64"}[v4])
		}
		b.WriteString("Address=" + cidr + "\n")
		if gw != "" {
			if g := net.ParseIP(gw); g == nil || (g.To4() != nil) != v4 {
				return fmt.Errorf("invalid %s gateway", label)
			}
			b.WriteString("Gateway=" + gw + "\n")
		}
		return nil
	}
	switch a.IPv4 {
	case "dhcp", "off":
	case "static":
		if err := static(a.Address, a.Gateway, true, "IPv4"); err != nil {
			return "", err
		}
	default:
		return "", errors.New("IPv4 mode must be dhcp, static or off")
	}
	switch a.IPv6 {
	case "auto", "dhcp":
		b.WriteString("IPv6AcceptRA=yes\n")
	case "static":
		if err := static(a.Address6, a.Gateway6, false, "IPv6"); err != nil {
			return "", err
		}
		b.WriteString("IPv6AcceptRA=no\n")
	case "off":
		b.WriteString("IPv6AcceptRA=no\n")
		if a.IPv4 == "off" {
			b.WriteString("LinkLocalAddressing=no\n")
		}
	default:
		return "", errors.New("IPv6 mode must be auto, dhcp, static or off")
	}
	for _, d := range a.DNS {
		if net.ParseIP(d) == nil {
			return "", fmt.Errorf("invalid DNS server %q", d)
		}
		b.WriteString("DNS=" + d + "\n")
	}
	if bridge {
		b.WriteString("ConfigureWithoutCarrier=yes\n")
	}
	if a.IPv4 == "dhcp" {
		b.WriteString("\n[DHCPv4]\nClientIdentifier=mac\n")
	}
	return b.String(), nil
}

func readMgmt() ([]string, bool) {
	b, err := os.ReadFile(mgmtFile)
	return strings.Fields(string(b)), err == nil
}

func LoadFirewall(ctx context.Context) error {
	names, ok := readMgmt()
	if !ok {
		_, _ = run(ctx, "nft", "delete", "table", "inet", "argus")
		return nil
	}
	allow := []string{`"lo"`, `"cni0"`}
	for _, n := range names {
		allow = append(allow, strconv.Quote(n))
	}
	rules := fmt.Sprintf("table inet argus\ndelete table inet argus\ntable inet argus {\n\tchain prerouting {\n\t\ttype filter hook prerouting priority -150; policy accept;\n\t\tiifname != { %s } fib daddr type local tcp dport { %s } drop\n\t}\n}\n", strings.Join(allow, ", "), mgmtPorts)
	if err := os.MkdirAll(filepath.Dir(firewallFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(firewallFile, []byte(rules), 0o600); err != nil {
		return err
	}
	_, err := run(ctx, "nft", "-f", firewallFile)
	return err
}

func (s *Server) setManagement(ctx context.Context, iface string, on bool) error {
	hn, err := hostNet(ctx)
	if err != nil {
		return err
	}
	var top []string
	found := false
	for _, i := range hn.Ifaces {
		if i.Master == "" {
			top = append(top, i.Name)
			found = found || i.Name == iface
		}
	}
	if !found {
		return fmt.Errorf("%s is not a configurable interface", iface)
	}
	current, restricted := readMgmt()
	if !restricted {
		current = top
	}
	next := slices.DeleteFunc(slices.Clone(current), func(n string) bool { return n == iface || !slices.Contains(top, n) })
	if on {
		next = append(next, iface)
	}
	for _, i := range hn.Ifaces {
		if i.Access && i.Master == "" && !slices.Contains(next, i.Name) {
			return fmt.Errorf("you are connected through %s; it must keep the management role", i.Name)
		}
	}
	if len(next) == 0 {
		return errors.New("at least one interface needs the management role")
	}
	sort.Strings(next)
	desc := fmt.Sprintf("management role %s %s", map[bool]string{true: "on", false: "off"}[on], iface)
	if len(next) == len(top) {
		return s.apply(ctx, desc, nil, []string{mgmtFile}, nil, nil)
	}
	return s.apply(ctx, desc, map[string]string{mgmtFile: strings.Join(next, "\n") + "\n"}, nil, nil, nil)
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
	if b, err := os.ReadFile(mgmtFile); err == nil {
		if err := os.WriteFile(filepath.Join(snapshotDir, filepath.Base(mgmtFile)), b, 0o644); err != nil {
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
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return errors.Join(err, Rollback(ctx))
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return errors.Join(err, Rollback(ctx))
		}
	}
	for _, path := range remove {
		os.Remove(path)
	}
	if err := LoadFirewall(ctx); err != nil {
		return errors.Join(err, Rollback(ctx))
	}
	if len(affected) == 0 {
		return nil
	}
	for _, c := range created {
		_, _ = run(ctx, "ip", "link", "del", c)
	}
	return reloadNetwork(ctx, affected)
}

func defaultRoutes(ctx context.Context, family string) ([]struct{ Gateway, Dev string }, error) {
	if family == "-6" && !ipv6Enabled() {
		return nil, nil
	}
	out, err := run(ctx, "ip", "-j", family, "route", "show", "default")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var routes []struct{ Gateway, Dev string }
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return nil, fmt.Errorf("default routes: %w", err)
	}
	return routes, nil
}

func ipv6Enabled() bool {
	_, err := os.Stat("/proc/net/if_inet6")
	return err == nil
}

func reloadNetwork(ctx context.Context, ifaces []string) error {
	if _, err := run(ctx, "networkctl", "reload"); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, i := range ifaces {
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join("/sys/class/net", i)); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if _, err := run(ctx, "networkctl", "reconfigure", i); err != nil {
			log.Printf("network: %v", err)
		}
	}
	return nil
}

func Confirm(ctx context.Context) error {
	if readPending() == nil {
		return errors.New("nothing to confirm")
	}
	if _, err := run(ctx, "systemctl", "stop", rollbackUnit+".timer"); err != nil && !strings.Contains(err.Error(), "not loaded") {
		return fmt.Errorf("could not cancel the rollback timer: %w", err)
	}
	os.RemoveAll(snapshotDir)
	return os.Remove(PendingFile)
}

func Rollback(ctx context.Context) error {
	p := readPending()
	if p == nil {
		return errors.New("nothing to roll back")
	}
	_, _ = run(ctx, "systemctl", "stop", rollbackUnit+".timer")
	var errs []error
	current, _ := filepath.Glob(filepath.Join(networkDir, managedGlob))
	for _, f := range current {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if err := os.Remove(mgmtFile); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	if b, err := os.ReadFile(filepath.Join(snapshotDir, filepath.Base(mgmtFile))); err == nil {
		errs = append(errs, os.WriteFile(mgmtFile, b, 0o644))
	}
	errs = append(errs, LoadFirewall(ctx))
	saved, _ := filepath.Glob(filepath.Join(snapshotDir, managedGlob))
	var ifaces []string
	for _, f := range saved {
		b, err := os.ReadFile(f)
		if err == nil {
			err = os.WriteFile(filepath.Join(networkDir, filepath.Base(f)), b, 0o644)
		}
		errs = append(errs, err)
	}
	for _, f := range append(current, saved...) {
		base := strings.TrimPrefix(filepath.Base(f), "01-argus-")
		ifaces = append(ifaces, strings.TrimSuffix(base, filepath.Ext(base)))
	}
	for _, c := range p.Created {
		_, _ = run(ctx, "ip", "link", "del", c)
	}
	errs = append(errs, reloadNetwork(ctx, ifaces))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("rollback incomplete, recovery files kept in %s: %w", snapshotDir, err)
	}
	os.RemoveAll(snapshotDir)
	return os.Remove(PendingFile)
}
