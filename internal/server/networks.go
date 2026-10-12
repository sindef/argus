package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func (s *Server) networks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.kube.ListNetworks(r.Context())
	attached := map[string]int{}
	if vms, verr := s.kube.ListVMs(r.Context()); verr == nil {
		for _, v := range vms {
			for _, n := range v.NICs {
				_, name, _ := strings.Cut(n.Network, "/")
				if name == "" {
					name = n.Network
				}
				attached[name]++
			}
		}
	}
	s.render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Networks": nets, "Attached": attached, "Bridges": s.bridges(r.Context()), "CanEdit": session(r).Role.Can(auth.Admin), "Error": errText(err)})
}

type networkReq struct {
	Name       string `json:"name"`
	VLAN       int    `json:"vlan"`
	Bridge     string `json:"bridge"`
	SpoofCheck bool   `json:"spoofCheck"`
	RateMbps   int    `json:"rateMbps"`
}

func (n networkReq) kube() kube.Network {
	return kube.Network{Name: n.Name, VLAN: n.VLAN, Bridge: n.Bridge, SpoofCheck: n.SpoofCheck, RateMbps: n.RateMbps}
}

func parseNetwork(r *http.Request) (networkReq, error) {
	n := networkReq{Name: r.FormValue("name"), Bridge: r.FormValue("bridge"), SpoofCheck: r.FormValue("spoofcheck") != ""}
	var err error
	if v := r.FormValue("vlan"); v != "" {
		n.VLAN, err = strconv.Atoi(v)
	}
	if v := r.FormValue("rate"); v != "" && err == nil {
		n.RateMbps, err = strconv.Atoi(v)
	}
	return n, err
}

func validNetwork(n networkReq) error {
	if n.VLAN < 0 || n.VLAN > 4094 {
		return errors.New("VLAN must be 1-4094")
	}
	if !validObject.MatchString(n.Name) {
		return errors.New("invalid name")
	}
	if n.RateMbps < 0 || n.RateMbps > 100000 {
		return errors.New("bandwidth limit must be 0-100000 Mbit/s")
	}
	return nil
}

func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	n, err := parseNetwork(r)
	if err == nil {
		err = validNetwork(n)
	}
	name, vlan := n.Name, n.VLAN
	if err == nil && n.Bridge != "" && !slices.Contains(s.bridges(r.Context()), n.Bridge) {
		err = fmt.Errorf("bridge %s not found", n.Bridge)
	}
	if err == nil && r.FormValue("edit") != "" {
		err = s.kube.UpdateNetwork(r.Context(), n.kube())
	} else if err == nil {
		err = s.kube.CreateNetwork(r.Context(), n.kube())
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: save network %s vlan %d", session(r).User, name, vlan)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.kube.DeleteNetwork(r.Context(), name); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: delete network %s", session(r).User, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) apiNetworkOp(w http.ResponseWriter, r *http.Request) {
	var n networkReq
	err := json.NewDecoder(r.Body).Decode(&n)
	if err == nil {
		err = validNetwork(n)
	}
	if err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	if r.PathValue("op") == "delete" {
		err = s.kube.DeleteNetwork(r.Context(), n.Name)
	} else if n.Bridge != "" && !slices.Contains(s.bridges(r.Context()), n.Bridge) {
		err = fmt.Errorf("bridge %s not found on %s", n.Bridge, s.nodeName)
	} else if r.PathValue("op") == "update" {
		err = s.kube.UpdateNetwork(r.Context(), n.kube())
	} else {
		err = s.kube.CreateNetwork(r.Context(), n.kube())
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: network %s %s vlan %d", session(r).User, r.PathValue("op"), n.Name, n.VLAN)
	writeJSON(w, map[string]string{})
}

func (s *Server) bridges(ctx context.Context) []string {
	if s.hostd == nil {
		return nil
	}
	hn, err := s.hostd.Network(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range hn.Ifaces {
		if i.Kind == "bridge" {
			out = append(out, i.Name)
		}
	}
	return out
}

func (s *Server) hostNet(r *http.Request) (hostd.HostNet, error) {
	if s.hostd == nil {
		return hostd.HostNet{}, errors.New("host helper not available")
	}
	return s.hostd.Network(hostd.WithAccess(r.Context(), r.Host))
}

func management(hn hostd.HostNet, i hostd.Iface) bool {
	if !hn.Restricted {
		return i.Master == ""
	}
	return i.Master == "" && slices.Contains(hn.Management, i.Name)
}

func (s *Server) hostNetworkPage(w http.ResponseWriter, r *http.Request) {
	info, err := s.ops(r).Network(accessCtx(r))
	var free []hostd.Iface
	for _, i := range info.Net.Ifaces {
		if i.Kind == "ether" && !i.Mgmt {
			free = append(free, i)
		}
	}
	s.render(w, r, "hostnet.html", "layout", map[string]any{"Nav": "hostnet", "Net": info.Net, "Ports": free, "Migration": info.Migration, "Error": errText(err)})
}

func (s *Server) ifaceModal(w http.ResponseWriter, r *http.Request) {
	info, err := s.ops(r).Network(accessCtx(r))
	hn := info.Net
	var iface *hostd.Iface
	for i := range hn.Ifaces {
		if hn.Ifaces[i].Name == r.PathValue("name") {
			iface = &hn.Ifaces[i]
		}
	}
	if err == nil && iface == nil {
		err = fmt.Errorf("interface %s not found", r.PathValue("name"))
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	s.render(w, r, "hostnet.html", "iface-modal", map[string]any{"I": iface, "DNS": hn.DNS, "Management": management(hn, *iface), "Migration": info.Migration == iface.Name})
}

func (s *Server) hostNetworkAction(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	req := netOpReq{
		Bridge: bridgeSpec(r),
		Delete: r.FormValue("bridge"),
		Address: hostd.AddressSpec{
			Iface: r.FormValue("iface"), IPv4: r.FormValue("ipv4"), IPv6: r.FormValue("ipv6"),
			Address: strings.TrimSpace(r.FormValue("address")), Gateway: strings.TrimSpace(r.FormValue("gateway")),
			Address6: strings.TrimSpace(r.FormValue("address6")), Gateway6: strings.TrimSpace(r.FormValue("gateway6")),
			DNS: fields(r.FormValue("dns")),
		},
		Iface:      r.FormValue("iface"),
		Migration:  r.FormValue("migration") != "",
		Management: r.FormValue("management") != "",
	}
	err := s.ops(r).NetworkOp(accessCtx(r), r.PathValue("op"), req)
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: host network %s %s", session(r).User, r.PathValue("op"), r.FormValue("iface"))
	w.Header().Set("HX-Refresh", "true")
}

type HostNetInfo struct {
	Net       hostd.HostNet `json:"net"`
	Migration string        `json:"migration"`
}

func (s *Server) apiHostNetwork(w http.ResponseWriter, r *http.Request) {
	hn, err := s.hostNet(r)
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, HostNetInfo{Net: hn, Migration: s.kube.MigrationBridge(r.Context())})
}

func bridgeSpec(r *http.Request) hostd.BridgeSpec {
	spec := hostd.BridgeSpec{Name: r.FormValue("name"), Ports: r.Form["ports"], BondMode: r.FormValue("bond_mode")}
	if len(spec.Ports) > 1 {
		spec.Bond = "bond-" + spec.Name
		spec.Bond = spec.Bond[:min(len(spec.Bond), 15)]
	}
	return spec
}
