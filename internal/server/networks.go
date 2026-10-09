package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"argus/internal/auth"

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
	s.render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Networks": nets, "Attached": attached, "CanEdit": session(r).Role.Can(auth.Admin), "Error": errText(err)})
}

type networkReq struct {
	Name string `json:"name"`
	VLAN int    `json:"vlan"`
}

func parseNetwork(r *http.Request) (networkReq, error) {
	n := networkReq{Name: r.FormValue("name")}
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
	} else {
		err = s.kube.CreateNetwork(r.Context(), n.Name, n.VLAN)
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
