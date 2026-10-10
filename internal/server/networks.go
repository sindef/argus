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
	Name   string `json:"name"`
	VLAN   int    `json:"vlan"`
	Bridge string `json:"bridge"`
}

func parseNetwork(r *http.Request) (networkReq, error) {
	n := networkReq{Name: r.FormValue("name"), Bridge: r.FormValue("bridge")}
	var err error
	if v := r.FormValue("vlan"); v != "" {
		n.VLAN, err = strconv.Atoi(v)
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
	if err == nil {
		err = s.kube.CreateNetwork(r.Context(), name, vlan, n.Bridge)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: create network %s vlan %d", session(r).User, name, vlan)
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
	} else {
		err = s.kube.CreateNetwork(r.Context(), n.Name, n.VLAN, n.Bridge)
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
	hn, err := s.hostNet(r)
	var free []hostd.Iface
	for _, i := range hn.Ifaces {
		if i.Kind == "ether" && !i.Mgmt {
			free = append(free, i)
		}
	}
	s.render(w, r, "hostnet.html", "layout", map[string]any{"Nav": "hostnet", "Net": hn, "Ports": free, "Migration": s.kube.MigrationBridge(r.Context()), "Error": errText(err)})
}

func (s *Server) ifaceModal(w http.ResponseWriter, r *http.Request) {
	hn, err := s.hostNet(r)
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
	s.render(w, r, "hostnet.html", "iface-modal", map[string]any{"I": iface, "DNS": hn.DNS, "Management": management(hn, *iface), "Migration": s.kube.MigrationBridge(r.Context()) == iface.Name})
}

func (s *Server) hostNetworkAction(w http.ResponseWriter, r *http.Request) {
	ctx := hostd.WithAccess(r.Context(), r.Host)
	var err error
	switch op := r.PathValue("op"); op {
	case "bridge":
		r.ParseForm()
		spec := hostd.BridgeSpec{Name: r.FormValue("name"), Ports: r.Form["ports"], Bond: r.FormValue("bond"), BondMode: r.FormValue("bond_mode")}
		err = s.hostd.CreateBridge(ctx, spec)
	case "delete":
		br := r.FormValue("bridge")
		nets, lerr := s.kube.ListNetworks(ctx)
		err = lerr
		for _, n := range nets {
			if err == nil && n.Bridge == br {
				err = fmt.Errorf("network %s uses %s; delete it first", n.Name, br)
			}
		}
		if err == nil && s.kube.MigrationBridge(ctx) == br {
			err = fmt.Errorf("%s carries live migration; move that role first", br)
		}
		if err == nil {
			err = s.hostd.DeleteBridge(ctx, br)
		}
	case "address":
		a := hostd.AddressSpec{
			Iface: r.FormValue("iface"), IPv4: r.FormValue("ipv4"), IPv6: r.FormValue("ipv6"),
			Address: strings.TrimSpace(r.FormValue("address")), Gateway: strings.TrimSpace(r.FormValue("gateway")),
			Address6: strings.TrimSpace(r.FormValue("address6")), Gateway6: strings.TrimSpace(r.FormValue("gateway6")),
			DNS: strings.Fields(strings.ReplaceAll(r.FormValue("dns"), ",", " ")),
		}
		err = s.hostd.SetAddress(ctx, a)
	case "roles":
		err = s.setRoles(r)
	case "confirm", "rollback":
		err = s.hostd.NetworkDecision(ctx, op)
	default:
		err = errors.New("unknown network operation")
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: host network %s %s", session(r).User, r.PathValue("op"), r.FormValue("iface"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) setRoles(r *http.Request) error {
	name := r.FormValue("iface")
	hn, err := s.hostNet(r)
	if err != nil {
		return err
	}
	var iface *hostd.Iface
	for i := range hn.Ifaces {
		if hn.Ifaces[i].Name == name {
			iface = &hn.Ifaces[i]
		}
	}
	if iface == nil || iface.Master != "" {
		return fmt.Errorf("%s cannot take roles", name)
	}
	if r.FormValue("migration") != "" && s.kube.MigrationBridge(r.Context()) != name {
		if iface.Kind != "bridge" {
			return errors.New("live migration needs a bridge")
		}
		if err := s.kube.SetMigrationBridge(r.Context(), name); err != nil {
			return err
		}
	}
	if on := r.FormValue("management") != ""; on != management(hn, *iface) {
		return s.hostd.SetManagement(hostd.WithAccess(r.Context(), r.Host), name, on)
	}
	return nil
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

func (s *Server) apiHostNetworkOp(w http.ResponseWriter, r *http.Request) {
	if s.hostd == nil {
		apiError(w, r, http.StatusServiceUnavailable, errors.New("host helper not available"))
		return
	}
	ctx := hostd.WithAccess(r.Context(), r.Host)
	var err error
	switch op := r.PathValue("op"); op {
	case "bridge":
		var spec hostd.BridgeSpec
		if err = json.NewDecoder(r.Body).Decode(&spec); err == nil {
			err = s.hostd.CreateBridge(ctx, spec)
		}
	case "confirm":
		err = s.hostd.NetworkDecision(ctx, op)
	default:
		err = errors.New("unknown network operation")
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: host network %s", session(r).User, r.PathValue("op"))
	writeJSON(w, map[string]string{})
}
