package hostserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/hostui"
	"argus/internal/ui"
	"argus/internal/wire"

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
	s.Render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Networks": nets, "Attached": attached, "Bridges": s.bridges(r.Context()), "CanEdit": ui.Session(r).Role.Can(auth.Admin), "Error": ui.ErrText(err)})
}

func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	n, err := hostui.ParseNetwork(r)
	if err == nil {
		err = hostui.ValidNetwork(n)
	}
	name, vlan := n.Name, n.VLAN
	if err == nil && n.Bridge != "" && !slices.Contains(s.bridges(r.Context()), n.Bridge) {
		err = fmt.Errorf("bridge %s not found", n.Bridge)
	}
	if err == nil && r.FormValue("edit") != "" {
		err = s.kube.UpdateNetwork(r.Context(), n.Kube())
	} else if err == nil {
		err = s.kube.CreateNetwork(r.Context(), n.Kube())
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: save network %s vlan %d", ui.Session(r).User, name, vlan)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.kube.DeleteNetwork(r.Context(), name); err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) apiNetworkOp(w http.ResponseWriter, r *http.Request) {
	var n wire.NetworkRequest
	err := json.NewDecoder(r.Body).Decode(&n)
	if err == nil {
		err = hostui.ValidNetwork(n)
	}
	if err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	switch {
	case r.PathValue("op") == "delete":
		err = s.kube.DeleteNetwork(r.Context(), n.Name)
	case n.Bridge != "" && !slices.Contains(s.bridges(r.Context()), n.Bridge):
		err = fmt.Errorf("bridge %s not found on %s", n.Bridge, s.NodeName)
	case r.PathValue("op") == "update":
		err = s.kube.UpdateNetwork(r.Context(), n.Kube())
	default:
		err = s.kube.CreateNetwork(r.Context(), n.Kube())
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: network %s %s vlan %d", ui.Session(r).User, r.PathValue("op"), n.Name, n.VLAN)
	ui.WriteJSON(w, map[string]string{})
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

func (s *Server) apiHostNetwork(w http.ResponseWriter, r *http.Request) {
	hn, err := s.hostNet(r)
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, hostui.HostNetInfo{Net: hn, Migration: s.kube.MigrationBridge(r.Context())})
}
