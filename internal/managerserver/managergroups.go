package managerserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"argus/internal/manager"
	"argus/internal/ui"
)

var groupTitles = map[string]string{
	"add-datacenter":    "New datacenter",
	"add-cluster":       "New cluster",
	"rename-datacenter": "Rename datacenter",
	"rename-cluster":    "Rename cluster",
	"move-host":         "Move host",
	"enter-maintenance": "Enter maintenance",
	"rolling-update":    "Rolling update",
	"cluster-settings":  "Cluster settings",
}

func (s *Server) groupNames(r *http.Request) ([]string, []string) {
	gs, _ := s.manager.Groups(r.Context(), ui.Session(r).Org)
	var dcs, cls []string
	for _, g := range gs {
		dcs = append(dcs, g.Datacenter)
		cls = append(cls, g.Clusters...)
	}
	return dcs, cls
}

func (s *Server) groupModal(w http.ResponseWriter, r *http.Request) {
	op := r.PathValue("op")
	title, ok := groupTitles[op]
	if !ok {
		http.NotFound(w, r)
		return
	}
	dcs, cls := s.groupNames(r)
	data := map[string]any{"Op": op, "Title": title, "DC": r.FormValue("dc"), "Cluster": r.FormValue("cluster"), "Host": r.FormValue("host"), "DCs": dcs, "Clusters": cls}
	switch op {
	case "rename-datacenter":
		data["Value"] = data["DC"]
	case "rename-cluster":
		data["Value"] = data["Cluster"]
	case "move-host", "enter-maintenance":
		data["Title"] = title + ": " + r.FormValue("host")
	case "rolling-update":
		data["Title"] = title + ": " + r.FormValue("cluster")
	case "cluster-settings":
		data["Title"] = title + ": " + r.FormValue("cluster")
		st, _ := s.manager.ClusterSettings(r.Context(), ui.Session(r).Org, r.FormValue("dc"), r.FormValue("cluster"))
		if st.Threshold == 0 {
			st.Threshold = 3
		}
		data["Settings"], data["Thresholds"] = st, []int{1, 2, 3, 4, 5}
		data["CPUModels"] = s.manager.CommonCPUModels(ui.Session(r).Org, r.FormValue("dc"), r.FormValue("cluster"))
	}
	s.Render(w, r, "manager.html", "group-modal", data)
}

func (s *Server) groupAction(w http.ResponseWriter, r *http.Request) {
	org, op := ui.Session(r).Org, r.PathValue("op")
	dc, cl, name := r.FormValue("dc"), r.FormValue("cluster"), strings.TrimSpace(r.FormValue("name"))
	var err error
	switch op {
	case "add-datacenter":
		err = s.manager.AddDatacenter(r.Context(), org, name)
	case "add-cluster":
		err = s.manager.AddCluster(r.Context(), org, dc, name)
	case "rename-datacenter":
		err = s.manager.RenameDatacenter(r.Context(), org, dc, name)
	case "rename-cluster":
		err = s.manager.RenameCluster(r.Context(), org, dc, cl, name)
	case "delete-datacenter":
		err = s.manager.DeleteGroup(r.Context(), org, dc, "")
	case "delete-cluster":
		err = s.manager.DeleteGroup(r.Context(), org, dc, cl)
	case "move-host":
		err = s.manager.MoveNode(r.Context(), org, r.FormValue("host"), strings.TrimSpace(r.FormValue("datacenter")), strings.TrimSpace(r.FormValue("cluster")))
	case "apply-networks":
		err = s.applyClusterNetworks(r, dc, cl)
	case "enter-maintenance":
		err = s.manager.EnterMaintenance(r.Context(), org, r.FormValue("host"), r.FormValue("shutdown") != "")
	case "exit-maintenance":
		err = s.manager.ExitMaintenance(r.Context(), org, r.FormValue("host"))
	case "cluster-settings":
		err = s.saveClusterSettings(r, dc, cl)
	case "rolling-update":
		img := strings.TrimSpace(r.FormValue("image"))
		if img != "" && !ui.ValidImage.MatchString(img) {
			err = errors.New("invalid image reference")
			break
		}
		err = s.manager.RollingUpdate(r.Context(), org, ungroup(dc), ungroup(cl), img, r.FormValue("shutdown") != "")
	default:
		err = errors.New("unknown operation")
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s %s", ui.Session(r).User, op, dc, cl, name)
	s.manager.PollAll(r.Context())
	w.Header().Set("HX-Refresh", "true")
}

func ungroup(name string) string {
	if name == "Unassigned" {
		return ""
	}
	return name
}

func inGroup(n manager.Node, dc, cl string) bool {
	return (dc == "" || groupName(n.Datacenter) == dc) && (cl == "" || groupName(n.Cluster) == cl)
}

func (s *Server) applyClusterNetworks(r *http.Request, dc, cl string) error {
	defs, err := s.manager.Networks(r.Context(), ui.Session(r).Org)
	if err != nil {
		return err
	}
	var members []manager.NodeState
	for _, st := range s.inventory(r) {
		if inGroup(st.Node, dc, cl) {
			members = append(members, st)
		}
	}
	if len(members) == 0 {
		return errors.New("no reachable hosts in this cluster")
	}
	errs := fanOut(r.Context(), members, func(ctx context.Context, n manager.Node) error {
		for _, d := range networksFor(defs, n) {
			if err := n.Call(ctx, http.MethodPost, "/api/networks/create", netReq(d), nil); err != nil {
				return err
			}
		}
		return nil
	})
	if errs != "" {
		return errors.New(errs)
	}
	return nil
}

func (s *Server) saveClusterSettings(r *http.Request, dc, cl string) error {
	org := ui.Session(r).Org
	st, err := s.manager.ClusterSettings(r.Context(), org, dc, cl)
	if err != nil {
		return err
	}
	st.CPUModel, st.DRS = r.FormValue("cpu_model"), r.FormValue("drs")
	if st.DRS != "" && st.DRS != "manual" && st.DRS != "auto" {
		return errors.New("invalid load balancing mode")
	}
	if st.Threshold, err = strconv.Atoi(r.FormValue("threshold")); err != nil || st.Threshold < 1 || st.Threshold > 5 {
		return errors.New("threshold must be 1-5")
	}
	if st.CPUModel != "" && !slices.Contains(s.manager.CommonCPUModels(org, dc, cl), st.CPUModel) {
		return fmt.Errorf("not every host in %s can run %s", cl, st.CPUModel)
	}
	return s.manager.SetClusterSettings(r.Context(), org, dc, cl, st)
}

func (s *Server) drsApply(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.ApplyRecommendation(r.Context(), ui.Session(r).Org, r.FormValue("id")); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: applied DRS recommendation %s", ui.Session(r).User, r.FormValue("id"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) drsRule(w http.ResponseWriter, r *http.Request) {
	if op := r.PathValue("op"); op != "add" && op != "delete" {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	org, dc, cl, name := ui.Session(r).Org, r.FormValue("dc"), r.FormValue("cluster"), r.FormValue("name")
	st, err := s.manager.ClusterSettings(r.Context(), org, dc, cl)
	if err == nil {
		st.Rules = slices.DeleteFunc(st.Rules, func(x manager.Rule) bool { return x.Name == name })
		if r.PathValue("op") == "add" {
			rule := manager.Rule{Name: name, Kind: r.FormValue("kind"), VMs: r.Form["vms"]}
			if rule.Kind == "hosts" {
				rule.Hosts = r.Form["hosts"]
			}
			_, kind := manager.RuleKinds[rule.Kind]
			switch {
			case !ui.ValidObject.MatchString(name):
				err = errors.New("invalid rule name")
			case !kind:
				err = errors.New("unknown rule type")
			case len(rule.VMs) < 1 || rule.Kind != "hosts" && len(rule.VMs) < 2:
				err = errors.New("select the VMs the rule covers")
			case rule.Kind == "hosts" && len(rule.Hosts) == 0:
				err = errors.New("select at least one host")
			}
			st.Rules = append(st.Rules, rule)
		}
	}
	if err == nil {
		err = s.manager.SetClusterSettings(r.Context(), org, dc, cl, st)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: DRS rule %s %s in %s/%s", ui.Session(r).User, r.PathValue("op"), name, dc, cl)
	w.Header().Set("HX-Refresh", "true")
}
