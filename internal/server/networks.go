package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"argus/internal/auth"
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
	s.render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Networks": nets, "Attached": attached, "CanEdit": session(r).Role.Can(auth.Admin), "Error": errText(err)})
}

func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	vlan := 0
	var err error
	if v := r.FormValue("vlan"); v != "" {
		if vlan, err = strconv.Atoi(v); err != nil || vlan < 1 || vlan > 4094 {
			err = errors.New("VLAN must be 1-4094")
		}
	}
	if err == nil && !validObject.MatchString(name) {
		err = errors.New("invalid name")
	}
	if err == nil {
		err = s.kube.CreateNetwork(r.Context(), name, vlan)
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
