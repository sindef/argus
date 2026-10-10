package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"argus/internal/auth"
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
	if u, perr := url.Parse(src); err == nil && (perr != nil || (u.Scheme != "http" && u.Scheme != "https")) {
		err = errors.New("URL must be http or https")
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
		if err == io.EOF {
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
		head, _ := body.Peek(isoMagicOffset + 5)
		if !diskImage(head) {
			return errors.New("not a disk image: use qcow2, raw with a partition table, ISO, VMDK, VHDX, VDI, or a gzip/xz-compressed image")
		}
		if err := s.kube.CreateImage(r.Context(), name, size, ""); err != nil {
			return err
		}
		log.Printf("%s: upload image %s", session(r).User, name)
		if err := s.kube.Upload(r.Context(), name, body); err != nil {
			s.kube.DeleteImage(context.Background(), name)
			return err
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

const isoMagicOffset = 32769

func diskImage(b []byte) bool {
	at := func(off int, magic string) bool {
		return len(b) >= off+len(magic) && string(b[off:off+len(magic)]) == magic
	}
	switch {
	case at(0, "QFI\xfb"), at(0, "KDMV"), at(0, "# Disk DescriptorFile"), at(0, "vhdxfile"), at(0, "conectix"),
		at(64, "\x7f\x10\xda\xbe"), at(isoMagicOffset, "CD001"),
		at(0, "\x1f\x8b"), at(0, "\xfd7zXZ\x00"),
		at(510, "\x55\xaa"), at(512, "EFI PART"):
		return true
	}
	return false
}
