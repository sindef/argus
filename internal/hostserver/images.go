package hostserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"argus/internal/lifecycle"
	"argus/internal/netguard"
	"argus/internal/registry"
	"argus/internal/ui"
	"argus/internal/wire"
)

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "table"
	}
	imgs, err := s.kube.ListImages(r.Context())
	s.Render(w, r, "images.html", tmpl, map[string]any{"Nav": "images", "Images": imgs, "Error": ui.ErrText(err)})
}

func (s *Server) importImage(w http.ResponseWriter, r *http.Request) {
	name, src := r.FormValue("name"), r.FormValue("url")
	size, err := ui.Gibibytes(r.FormValue("size"))
	if err == nil && !ui.ValidObject.MatchString(name) {
		err = errors.New("invalid name")
	}
	if err == nil {
		err = netguard.CheckURL(r.Context(), src)
	}
	if err == nil {
		err = s.kube.CreateImage(r.Context(), name, size, src)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: import image %s from %s", ui.Session(r).User, name, src)
	s.trackImage(r.Context(), "Import image", name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) uploadImage(w http.ResponseWriter, r *http.Request) {
	err := s.streamUpload(r)
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) streamUpload(r *http.Request) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return errors.New("no file")
		}
		if err != nil {
			return err
		}
		if part.FormName() != "file" {
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			fields[part.FormName()] = string(b)
			continue
		}
		name := fields["name"]
		size, err := ui.Gibibytes(fields["size"])
		if err != nil {
			return err
		}
		if !ui.ValidObject.MatchString(name) {
			return errors.New("invalid name")
		}
		body := bufio.NewReaderSize(part, 64<<10)
		head, _ := body.Peek(registry.SniffLen)
		if !diskImage(head) {
			return errors.New("not a disk image: use qcow2, raw with a partition table, ISO, VMDK, VHD, VHDX, VDI, or a gzip/xz-compressed image")
		}
		if err := s.kube.CreateImage(r.Context(), name, size, ""); err != nil {
			return err
		}
		log.Printf("%s: upload image %s", ui.Session(r).User, name)
		if err := s.kube.Upload(r.Context(), name, body); err != nil {
			cctx, cancel := lifecycle.CleanupContext()
			defer cancel()
			return errors.Join(err, s.kube.DeleteImage(cctx, name))
		}
		return nil
	}
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.kube.DeleteImage(r.Context(), name); err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func diskImage(b []byte) bool {
	return registry.IsDisk(b) || bytes.HasPrefix(b, []byte("\x1f\x8b")) || bytes.HasPrefix(b, []byte("\xfd7zXZ\x00"))
}

func (s *Server) apiImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.kube.ListImages(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, imgs)
}

func (s *Server) apiImportImage(w http.ResponseWriter, r *http.Request) {
	var req wire.ImageImportRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	switch {
	case err != nil || !ui.ValidObject.MatchString(req.Name) || req.Size == "":
		err = errors.New("name and size required")
	case req.Export != nil:
		err = s.kube.ImportImage(r.Context(), req.Name, req.Size, *req.Export)
	case req.Registry != nil && req.Registry.Image != "":
		host, _, _ := strings.Cut(req.Registry.Image, "/")
		if err = netguard.CheckHost(r.Context(), host); err == nil {
			err = s.kube.ImportRegistryImage(r.Context(), req.Name, req.Size, *req.Registry)
		}
	case strings.HasPrefix(req.URL, "https://") || strings.HasPrefix(req.URL, "http://"):
		if err = netguard.CheckURL(r.Context(), req.URL); err == nil {
			err = s.kube.CreateImage(r.Context(), req.Name, req.Size, req.URL)
		}
	default:
		err = errors.New("url, export or registry required")
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: import image %s", ui.Session(r).User, req.Name)
	s.trackImage(r.Context(), "Import image", req.Name)
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiImageOp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var err error
	var out any = map[string]string{}
	switch r.PathValue("op") {
	case "export":
		var req wire.ExportRequest
		if err = ui.DecodeJSON(r.Body, &req); err == nil {
			out, err = s.kube.ExportImage(r.Context(), name, req.BaseURL, string(s.caPEM))
		}
	case "unexport":
		err = s.kube.DeleteImageExport(r.Context(), name)
	case "imported":
		err = s.kube.FinishImageImport(r.Context(), name)
	case "delete":
		err = s.kube.DeleteImage(r.Context(), name)
	default:
		err = errors.New("unknown operation")
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSON(w, out)
}
