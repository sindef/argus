package server

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
)

type v1Image struct {
	Name  string `json:"name"`
	Size  string `json:"size"`
	Phase string `json:"phase"`
}

func (s *Server) v1Images(w http.ResponseWriter, r *http.Request) {
	n, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	st, _ := s.manager.NodeState(n.Name)
	out := []v1Image{}
	for _, img := range st.Images {
		out = append(out, v1Image{img.Name, img.Size, img.Phase})
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1UpdateVM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cores      *int64             `json:"cores"`
		MemoryMiB  *int64             `json:"memoryMiB"`
		Tags       *[]string          `json:"tags"`
		Folder     *string            `json:"folder"`
		Attributes *map[string]string `json:"attributes"`
	}
	if err := ui.DecodeJSON(r.Body, &req); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	n, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	if s.manager.Busy(n.Name, r.PathValue("name")) {
		ui.APIError(w, r, http.StatusConflict, errors.New("a migration of this VM is in progress"))
		return
	}
	ctx, ns, name, ops := r.Context(), kube.Namespace, r.PathValue("name"), remoteOps{n}
	if req.Cores != nil || req.MemoryMiB != nil {
		hw, err := ops.Hardware(ctx, ns, name)
		if err == nil {
			if req.Cores != nil {
				hw.Cores = *req.Cores
				if hw.CoresPerSocket <= 0 || hw.Cores%hw.CoresPerSocket != 0 {
					hw.CoresPerSocket = hw.Cores
				}
			}
			if req.MemoryMiB != nil {
				hw.MemoryMiB = *req.MemoryMiB
			}
			switch {
			case hw.Cores < 1 || hw.Cores > 64:
				err = errors.New("cores must be 1-64")
			case hw.MemoryMiB < 128 || hw.MemoryMiB > 262144:
				err = errors.New("memory must be 128-262144 MiB")
			default:
				hw.DeviceChoices = nil
				err = ops.HardwareOp(ctx, ns, name, "save", hostui.HWOpReq{Hardware: hw})
			}
		}
		if err != nil {
			ui.APIError(w, r, http.StatusConflict, err)
			return
		}
	}
	var vm kube.VM
	if err := n.Call(ctx, http.MethodGet, manager.APIPath("/api/vms/%s/%s", ns, name), nil, &vm); err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	if req.Tags != nil || req.Folder != nil || req.Attributes != nil {
		tags, folder, attrs := vm.Tags, vm.Folder, vm.Attributes
		if req.Tags != nil {
			tags = *req.Tags
		}
		if req.Folder != nil {
			folder = *req.Folder
		}
		if req.Attributes != nil {
			attrs = *req.Attributes
		}
		var lines []string
		for k, v := range attrs {
			lines = append(lines, k+"="+v)
		}
		slices.Sort(lines)
		body := map[string]string{"tags": strings.Join(tags, ","), "folder": folder, "attributes": strings.Join(lines, "\n")}
		if err := n.Call(ctx, http.MethodPost, manager.APIPath("/api/vms/%s/%s/tags", ns, name), body, nil); err != nil {
			ui.APIError(w, r, http.StatusConflict, err)
			return
		}
		if err := n.Call(ctx, http.MethodGet, manager.APIPath("/api/vms/%s/%s", ns, name), nil, &vm); err != nil {
			ui.APIError(w, r, http.StatusBadGateway, err)
			return
		}
	}
	go s.manager.Refresh(n.Name)
	ui.WriteJSON(w, toV1VM(n.Name, vm))
}

func (s *Server) v1Networks(w http.ResponseWriter, r *http.Request) {
	ns, err := s.manager.Networks(r.Context(), ui.Session(r).Org)
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, append([]manager.Network{}, ns...))
}

func (s *Server) v1Network(w http.ResponseWriter, r *http.Request) {
	ns, err := s.manager.Networks(r.Context(), ui.Session(r).Org)
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	i := slices.IndexFunc(ns, func(n manager.Network) bool { return n.Name == r.PathValue("name") })
	if i < 0 {
		ui.APIError(w, r, http.StatusNotFound, errors.New("network not found"))
		return
	}
	ui.WriteJSON(w, ns[i])
}

func (s *Server) v1SaveNetwork(w http.ResponseWriter, r *http.Request) {
	var n manager.Network
	if err := ui.DecodeJSON(r.Body, &n); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	op := "create"
	if r.Method == http.MethodPut {
		op, n.Name = "update", r.PathValue("name")
	}
	if err := checkNetworkDef(n); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.manager.SaveNetwork(r.Context(), ui.Session(r).Org, n, op == "update"); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	out := map[string]any{"network": n}
	if errs := s.pushNetwork(r, op, n); errs != "" {
		out["warning"] = "not applied on every host: " + errs
	}
	ui.WriteJSONCode(w, map[bool]int{true: http.StatusCreated, false: http.StatusOK}[op == "create"], out)
}

func (s *Server) v1DeleteNetwork(w http.ResponseWriter, r *http.Request) {
	n := manager.Network{Name: r.PathValue("name")}
	if errs := s.pushNetwork(r, "delete", n); errs != "" {
		ui.APIError(w, r, http.StatusConflict, errors.New(errs))
		return
	}
	if err := s.manager.DeleteNetwork(r.Context(), ui.Session(r).Org, n.Name); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
