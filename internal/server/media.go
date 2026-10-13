package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"argus/internal/hostui"
	"argus/internal/lifecycle"
	"argus/internal/manager"
	"argus/internal/registry"
	"argus/internal/ui"
)

func mediaImage(vm string) string {
	return vm[:min(len(vm), 57)] + "-media"
}

func (l localOps) MountMedia(ctx context.Context, ns, name, contentType string, size int64, body io.Reader) error {
	s := l.s
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return errors.New("expected a multipart upload")
	}
	part, err := multipart.NewReader(body, params["boundary"]).NextPart()
	if err != nil || part.FormName() != "file" {
		return errors.New("no file")
	}
	in := bufio.NewReaderSize(part, 64<<10)
	if head, _ := in.Peek(registry.SniffLen); !diskImage(head) {
		return errors.New("not a disk image")
	}
	if err := l.EjectMedia(ctx, ns, name); err != nil {
		return err
	}
	img := mediaImage(name)
	if err := s.kube.CreateImage(ctx, img, fmt.Sprintf("%dGi", size>>30+1), ""); err != nil {
		return err
	}
	if err := s.kube.Upload(ctx, img, in); err != nil {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		return errors.Join(err, s.kube.DeleteImage(cctx, img))
	}
	return s.kube.SetCDROM(ctx, ns, name, img)
}

func (l localOps) EjectMedia(ctx context.Context, ns, name string) error {
	if err := l.s.kube.SetCDROM(ctx, ns, name, ""); err != nil {
		return err
	}
	if err := l.s.kube.DeleteImage(ctx, mediaImage(name)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (o remoteOps) MountMedia(ctx context.Context, ns, name, contentType string, size int64, body io.Reader) error {
	resp, err := o.n.Open(ctx, http.MethodPost, manager.APIPath("/api/vms/%s/%s/media", ns, name), http.Header{"Content-Type": {contentType}, "X-Media-Size": {fmt.Sprint(size)}}, body)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (o remoteOps) EjectMedia(ctx context.Context, ns, name string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/media/eject", ns, name), nil)
}

func (s *Server) apiMedia(w http.ResponseWriter, r *http.Request) {
	l := localOps{s}
	var err error
	if r.PathValue("op") == "eject" {
		err = l.EjectMedia(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	} else {
		err = l.MountMedia(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.Header.Get("Content-Type"), hostui.MediaSize(r), r.Body)
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}
