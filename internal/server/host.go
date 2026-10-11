package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"argus/internal/hostd"
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
	reason := ""
	if err := s.kube.PlatformReady(r.Context()); err != nil {
		reason = err.Error()
	} else if s.hostd != nil {
		if ps, err := s.hostd.Pools(r.Context()); err != nil {
			reason = err.Error()
		} else {
			for _, p := range ps.Pools {
				if p.Health != "ONLINE" {
					reason = "pool " + p.Name + " is " + p.Health
				}
			}
		}
	}
	writeJSON(w, map[string]any{"status": strings.TrimSpace(readFile(filepath.Join(s.runDir, "status"))), "version": readEnv(s.releaseFile)["KAIROS_VERSION"], "running": n, "ready": reason == "", "reason": reason})
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
	if st, serr := s.hostSettings(r.Context()); serr == nil {
		data["Settings"] = st
	}
	data["Notify"], _, _ = s.loadNotify(r.Context())
	data["Cert"] = s.certInfo()
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

type HostSettings struct {
	hostd.Settings
	MigrationBridge string   `json:"migrationBridge"`
	Bridges         []string `json:"bridges"`
}

type HostSettingsUpdate struct {
	hostd.SettingsUpdate
	MigrationBridge *string `json:"migrationBridge,omitempty"`
}

func (s *Server) hostSettings(ctx context.Context) (HostSettings, error) {
	if s.hostd == nil {
		return HostSettings{}, errors.New("host helper not available")
	}
	st, err := s.hostd.Settings(ctx)
	return HostSettings{Settings: st, MigrationBridge: s.kube.MigrationBridge(ctx), Bridges: s.bridges(ctx)}, err
}

func (s *Server) applyHostSettings(ctx context.Context, u HostSettingsUpdate) error {
	if s.hostd == nil {
		return errors.New("host helper not available")
	}
	if err := s.hostd.ApplySettings(ctx, u.SettingsUpdate); err != nil {
		return err
	}
	if u.MigrationBridge != nil && *u.MigrationBridge != s.kube.MigrationBridge(ctx) {
		if !slices.Contains(s.bridges(ctx), *u.MigrationBridge) {
			return fmt.Errorf("bridge %s not found on %s", *u.MigrationBridge, s.nodeName)
		}
		return s.kube.SetMigrationBridge(ctx, *u.MigrationBridge)
	}
	return nil
}

func (s *Server) apiHostSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.hostSettings(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) apiApplyHostSettings(w http.ResponseWriter, r *http.Request) {
	var u HostSettingsUpdate
	err := json.NewDecoder(r.Body).Decode(&u)
	if err == nil {
		err = s.applyHostSettings(r.Context(), u)
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: host settings applied", session(r).User)
	writeJSON(w, map[string]string{})
}

func fields(v string) []string { return strings.Fields(strings.ReplaceAll(v, ",", " ")) }

func (s *Server) hostSettingsForm(w http.ResponseWriter, r *http.Request) {
	dns, ntp, syslog, backup := fields(r.FormValue("dns")), fields(r.FormValue("ntp")), strings.TrimSpace(r.FormValue("syslog")), strings.TrimSpace(r.FormValue("backup"))
	u := HostSettingsUpdate{SettingsUpdate: hostd.SettingsUpdate{DNS: &dns, NTP: &ntp, Syslog: &syslog, Backup: &backup}}
	if err := s.applyHostSettings(r.Context(), u); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: host settings saved", session(r).User)
	w.Header().Set("HX-Refresh", "true")
}

type certInfo struct {
	Custom  bool
	Subject string
	Expires time.Time
}

func (s *Server) certInfo() certInfo {
	if s.certs == nil {
		return certInfo{}
	}
	if c := s.certs.Custom(); c != nil {
		names := append([]string{}, c.DNSNames...)
		for _, ip := range c.IPAddresses {
			names = append(names, ip.String())
		}
		return certInfo{Custom: true, Subject: strings.Join(names, ", "), Expires: c.NotAfter}
	}
	return certInfo{}
}

func (s *Server) setCertificate(w http.ResponseWriter, r *http.Request) {
	if s.manager != nil && !superAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var err error
	if s.certs == nil {
		err = errors.New("certificates are not managed by this server")
	} else if r.FormValue("revert") != "" {
		err = s.certs.Set(r.Context(), s.auth.Secrets(), nil, nil)
	} else {
		var cert, key []byte
		if cert, err = formFile(r, "cert"); err == nil {
			key, err = formFile(r, "key")
		}
		if err == nil {
			err = s.certs.Set(r.Context(), s.auth.Secrets(), cert, key)
		}
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: web certificate updated", session(r).User)
	w.Header().Set("HX-Refresh", "true")
}

func formFile(r *http.Request, name string) ([]byte, error) {
	f, _, err := r.FormFile(name)
	if err != nil {
		return nil, fmt.Errorf("choose a %s file", name)
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
