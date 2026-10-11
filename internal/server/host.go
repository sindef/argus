package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"argus/internal/tlscert"
)

var validImage = regexp.MustCompile(`^[A-Za-z0-9./:@_-]+$`)

func (s *Server) host(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "" {
		s.renderHost(w, r, "", nil)
		return
	}
	s.render(w, r, "host.html", "state", map[string]any{
		"Release": readEnv(s.releaseFile),
		"Status":  strings.TrimSpace(readFile(filepath.Join(s.runDir, "status"))),
		"Log":     tail(readFile(filepath.Join(s.runDir, "upgrade.log")), 15),
	})
}

func (s *Server) requestHost(r *http.Request, action, img string) error {
	req := action
	switch action {
	case "reboot", "poweroff":
	case "upgrade":
		if !validImage.MatchString(img) {
			return errors.New("invalid image reference")
		}
		req += " " + img
	default:
		return errors.New("unknown action")
	}
	if s.runDir == "" {
		return errors.New("host control not available")
	}
	if err := s.maintenance(); err != nil {
		return err
	}
	n, err := s.kube.RunningVMs(r.Context())
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("power off all VMs first (%d running)", n)
	}
	tmp := filepath.Join(s.runDir, ".request")
	if err := os.WriteFile(tmp, []byte(req+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.runDir, "request")); err != nil {
		return err
	}
	log.Printf("%s: host %s", session(r).User, req)
	return nil
}

func (s *Server) hostRequest(w http.ResponseWriter, r *http.Request) {
	if err := s.requestHost(r, r.PathValue("action"), strings.TrimSpace(r.FormValue("image"))); err != nil {
		flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) apiHostRequest(w http.ResponseWriter, r *http.Request) {
	var req struct{ Image string }
	json.NewDecoder(r.Body).Decode(&req)
	if err := s.requestHost(r, r.PathValue("action"), strings.TrimSpace(req.Image)); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]string{})
}

func (s *Server) apiHostStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.kube.RunningVMs(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]any{"status": strings.TrimSpace(readFile(filepath.Join(s.runDir, "status"))), "version": readEnv(s.releaseFile)["KAIROS_VERSION"], "running": n})
}

var errMaintenance = errors.New("host maintenance in progress")

func (s *Server) maintenance() error {
	if s.runDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.runDir, "request")); err == nil {
		return errMaintenance
	}
	status := readFile(filepath.Join(s.runDir, "status"))
	for _, p := range []string{"Updating", "Rebooting", "Shutting down"} {
		if strings.HasPrefix(status, p) {
			return errMaintenance
		}
	}
	return nil
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func readEnv(path string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(readFile(path)))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = strings.Trim(v, `"`)
		}
	}
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (s *Server) renderHost(w http.ResponseWriter, r *http.Request, notice string, err error) {
	tokens, terr := s.auth.Tokens(r.Context())
	if err == nil {
		err = terr
	}
	data := map[string]any{
		"Nav":           "host",
		"Available":     s.runDir != "",
		"Tokens":        tokens,
		"CAFingerprint": tlscert.Fingerprint(s.caPEM),
		"Notice":        notice,
		"Error":         errText(err),
	}
	if s.runDir != "" {
		data["Release"] = readEnv(s.releaseFile)
		data["Status"] = strings.TrimSpace(readFile(filepath.Join(s.runDir, "status")))
		data["Log"] = tail(readFile(filepath.Join(s.runDir, "upgrade.log")), 15)
	}
	s.render(w, r, "host.html", "layout", data)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if !validName.MatchString(name) {
		s.renderHost(w, r, "", errors.New("invalid token name"))
		return
	}
	tok, err := s.auth.CreateToken(r.Context(), name)
	if err != nil {
		s.renderHost(w, r, "", err)
		return
	}
	log.Printf("%s: created API token %s", session(r).User, name)
	s.renderHost(w, r, "Token "+name+": "+tok, nil)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.auth.DeleteToken(r.Context(), name); err != nil {
		s.renderHost(w, r, "", err)
		return
	}
	log.Printf("%s: revoked API token %s", session(r).User, name)
	http.Redirect(w, r, "/host", http.StatusSeeOther)
}
