package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/netguard"
	"argus/internal/registry"
	"argus/internal/wire"
)

var validObject = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func gibibytes(v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 4096 {
		return "", errors.New("size must be 1-4096 GiB")
	}
	return fmt.Sprintf("%dGi", n), nil
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "table"
	}
	imgs, err := s.kube.ListImages(r.Context())
	s.render(w, r, "images.html", tmpl, map[string]any{"Nav": "images", "Images": imgs, "CanOperate": session(r).Role.Can(auth.Operator), "Error": errText(err)})
}

func (s *Server) importImage(w http.ResponseWriter, r *http.Request) {
	name, src := r.FormValue("name"), r.FormValue("url")
	size, err := gibibytes(r.FormValue("size"))
	if err == nil && !validObject.MatchString(name) {
		err = errors.New("invalid name")
	}
	if err == nil {
		err = netguard.CheckURL(r.Context(), src)
	}
	if err == nil {
		err = s.kube.CreateImage(r.Context(), name, size, src)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: import image %s from %s", session(r).User, name, src)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) uploadImage(w http.ResponseWriter, r *http.Request) {
	err := s.streamUpload(r)
	if err != nil {
		flash(w, r, err)
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
		size, err := gibibytes(fields["size"])
		if err != nil {
			return err
		}
		if !validObject.MatchString(name) {
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
		log.Printf("%s: upload image %s", session(r).User, name)
		if err := s.kube.Upload(r.Context(), name, body); err != nil {
			cctx, cancel := cleanupCtx()
			defer cancel()
			return errors.Join(err, s.kube.DeleteImage(cctx, name))
		}
		return nil
	}
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.kube.DeleteImage(r.Context(), name); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: delete image %s", session(r).User, name)
	w.Header().Set("HX-Refresh", "true")
}

func diskImage(b []byte) bool {
	return registry.IsDisk(b) || bytes.HasPrefix(b, []byte("\x1f\x8b")) || bytes.HasPrefix(b, []byte("\xfd7zXZ\x00"))
}

func (s *Server) apiImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.kube.ListImages(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, imgs)
}

type imageImportReq struct {
	Name     string               `json:"name"`
	Size     string               `json:"size"`
	URL      string               `json:"url"`
	Export   *kube.Export         `json:"export"`
	Registry *kube.RegistrySource `json:"registry"`
}

func (s *Server) apiImportImage(w http.ResponseWriter, r *http.Request) {
	var req imageImportReq
	err := json.NewDecoder(r.Body).Decode(&req)
	switch {
	case err != nil || !validObject.MatchString(req.Name) || req.Size == "":
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
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: import image %s", session(r).User, req.Name)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiImageOp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var err error
	var out any = map[string]string{}
	switch r.PathValue("op") {
	case "export":
		var req wire.ExportRequest
		if err = decodeJSON(r.Body, &req); err == nil {
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
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, out)
}
