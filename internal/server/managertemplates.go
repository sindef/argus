package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"argus/internal/kube"
	"argus/internal/manager"
)

type libTemplate struct {
	Name       string         `json:"name"`
	SourceVM   string         `json:"sourceVM"`
	SourceHost string         `json:"sourceHost"`
	Arch       string         `json:"arch"`
	Created    time.Time      `json:"created"`
	Spec       map[string]any `json:"spec"`
	Disks      []libDisk      `json:"disks"`
}

type libDisk struct {
	DataVolume string `json:"dataVolume"`
	Size       int64  `json:"size"`
}

func (t libTemplate) Bytes() int64 {
	var n int64
	for _, d := range t.Disks {
		n += d.Size
	}
	return n
}

func (s *Server) templateDir(org, name string) string {
	return filepath.Join(s.dataDir, "templates", org, name)
}

func (s *Server) libraryTemplates(org string) []libTemplate {
	dirs, _ := os.ReadDir(filepath.Join(s.dataDir, "templates", org))
	var out []libTemplate
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(s.templateDir(org, d.Name()), "template.json"))
		var t libTemplate
		if err == nil && json.Unmarshal(b, &t) == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) libraryTemplate(org, name string) (libTemplate, error) {
	i := slices.IndexFunc(s.libraryTemplates(org), func(t libTemplate) bool { return t.Name == name })
	if i < 0 {
		return libTemplate{}, fmt.Errorf("template %s not found", name)
	}
	return s.libraryTemplates(org)[i], nil
}

func (s *Server) saveToLibrary(org string, src manager.Node, ns, vm, name string) error {
	if !validObject.MatchString(name) {
		return errors.New("invalid template name")
	}
	dir := s.templateDir(org, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("template %s already exists", name)
	}
	st, _ := s.manager.NodeState(src.Name)
	return s.manager.Task(org, "Save template "+name+" to Argus Manager", "library-template:"+org+"/"+name, func(ctx context.Context, progress func(string)) error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		ok := false
		defer func() {
			if !ok {
				os.RemoveAll(dir)
			}
		}()
		progress("Exporting")
		var ex kube.Export
		if err := src.Call(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/snapshot-export", ns, vm), map[string]string{"baseURL": src.URL}, &ex); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			src.Call(cctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/unexport", ns, vm), nil, nil)
		}()
		t := libTemplate{Name: name, SourceVM: vm, SourceHost: src.Name, Arch: st.Arch(), Created: time.Now(), Spec: ex.Spec}
		for _, d := range ex.Disks {
			if err := freeSpace(s.dataDir, 0); err != nil {
				return err
			}
			resp, err := src.Stream(ctx, d.URL, http.Header{"X-Kubevirt-Export-Token": {ex.Token}})
			if err != nil {
				return fmt.Errorf("disk %s: %w", d.DataVolume, err)
			}
			f, err := os.Create(filepath.Join(dir, d.DataVolume+".img.gz"))
			if err != nil {
				resp.Body.Close()
				return err
			}
			pr := &progressReader{r: resp.Body, last: time.Now(), fn: func(n int64) { progress(fmt.Sprintf("Copying %s: %.1f GiB", d.DataVolume, float64(n)/gib)) }}
			n, err := io.Copy(f, pr)
			resp.Body.Close()
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("disk %s: %w", d.DataVolume, err)
			}
			t.Disks = append(t.Disks, libDisk{d.DataVolume, n})
		}
		b, _ := json.Marshal(t)
		if err := os.WriteFile(filepath.Join(dir, "template.json"), b, 0o600); err != nil {
			return err
		}
		ok = true
		progress("Saved")
		return nil
	})
}

func (s *Server) deployFromLibrary(r *http.Request, name string, dst manager.Node, vm, pool string) error {
	org := session(r).Org
	t, err := s.libraryTemplate(org, name)
	if err != nil {
		return err
	}
	addr := s.registryAddress(r.Context())
	if addr == "" {
		return errors.New("set the address hosts use to reach Argus Manager under Images → Image sources first")
	}
	var spec map[string]any
	b, _ := json.Marshal(t.Spec)
	json.Unmarshal(b, &spec)
	_, token := s.newPull("template:" + org + "/" + name)
	ex := kube.Export{Spec: spec, Token: token, CA: string(s.caPEM)}
	for _, d := range t.Disks {
		ex.Disks = append(ex.Disks, kube.ExportDisk{DataVolume: d.DataVolume, URL: fmt.Sprintf("https://%s/template-data/%s/%s/%s.img.gz", addr, org, name, d.DataVolume)})
	}
	manager.RenameExport(&ex, t.SourceVM, vm)
	return s.manager.Deploy(org, dst, vm, pool, name, ex)
}

func (s *Server) serveTemplateData(w http.ResponseWriter, r *http.Request) {
	org, name, file := r.PathValue("org"), r.PathValue("name"), r.PathValue("file")
	s.regAuth.mu.Lock()
	g, ok := s.regAuth.pulls[secretKey(strings.TrimSpace(r.Header.Get("x-kubevirt-export-token")))]
	s.regAuth.mu.Unlock()
	if !ok || time.Now().After(g.expires) || g.repo != "template:"+org+"/"+name {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	t, err := s.libraryTemplate(org, name)
	if err != nil || !slices.ContainsFunc(t.Disks, func(d libDisk) bool { return d.DataVolume+".img.gz" == file }) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(s.templateDir(org, name), file))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", st.ModTime(), f)
	log.Printf("template %s/%s %s served", org, name, file)
}

type hostTemplate struct {
	kube.VM
	Host string
}

func (s *Server) hostTemplates(r *http.Request) []hostTemplate {
	var out []hostTemplate
	for _, st := range s.inventory(r) {
		for _, v := range st.VMs {
			if v.Template {
				out = append(out, hostTemplate{v, st.Node.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) vmCopyModal(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "manager-images.html", "vm-copy", map[string]any{"Mode": r.FormValue("mode"), "Host": r.FormValue("host"), "NS": r.FormValue("ns"), "VM": r.FormValue("vm"),
		"Source": r.FormValue("source"), "Hosts": s.managerData(r)["Hosts"]})
}

func (s *Server) vmCopy(w http.ResponseWriter, r *http.Request) {
	org := session(r).Org
	mode, name, target, pool := r.FormValue("mode"), strings.TrimSpace(r.FormValue("name")), r.FormValue("target"), strings.TrimSpace(r.FormValue("pool"))
	err := error(nil)
	if !validObject.MatchString(name) {
		err = errors.New("name must be lowercase letters, digits and dashes")
	}
	var dst manager.Node
	if err == nil && target != "manager" {
		dst, err = s.nodeIn(r, target)
	}
	if err == nil {
		switch mode {
		case "deploy-library":
			err = s.deployFromLibrary(r, r.FormValue("vm"), dst, name, pool)
		default:
			var src manager.Node
			src, err = s.nodeIn(r, r.FormValue("host"))
			switch {
			case err != nil:
			case target == "manager":
				err = s.saveToLibrary(org, src, r.FormValue("ns"), r.FormValue("vm"), name)
			default:
				err = s.manager.CopyVM(src, dst, r.FormValue("ns"), r.FormValue("vm"), name, pool, mode == "template")
			}
		}
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s to %s as %s", session(r).User, mode, r.FormValue("host"), r.FormValue("vm"), target, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteLibraryTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if _, err := s.libraryTemplate(session(r).Org, name); err != nil {
		flash(w, r, err)
		return
	}
	if err := os.RemoveAll(s.templateDir(session(r).Org, name)); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: deleted template %s", session(r).User, name)
	w.Header().Set("HX-Refresh", "true")
}
