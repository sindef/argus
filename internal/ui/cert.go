package ui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type certInfo struct {
	Custom  bool
	Subject string
	Expires time.Time
}

func (s *Core) CertInfo() certInfo {
	if s.Certs == nil {
		return certInfo{}
	}
	if c := s.Certs.Custom(); c != nil {
		names := append([]string{}, c.DNSNames...)
		for _, ip := range c.IPAddresses {
			names = append(names, ip.String())
		}
		return certInfo{Custom: true, Subject: strings.Join(names, ", "), Expires: c.NotAfter}
	}
	return certInfo{}
}

func (s *Core) setCertificate(w http.ResponseWriter, r *http.Request) {
	if !s.Mode.Super(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var err error
	switch {
	case s.Certs == nil:
		err = errors.New("certificates are not managed by this server")
	case r.FormValue("revert") != "":
		err = s.Certs.Set(r.Context(), s.Auth.Secrets(), nil, nil)
	default:
		var cert, key []byte
		if cert, err = FormFile(r, "cert"); err == nil {
			key, err = FormFile(r, "key")
		}
		if err == nil {
			err = s.Certs.Set(r.Context(), s.Auth.Secrets(), cert, key)
		}
	}
	if err != nil {
		Flash(w, r, err)
		return
	}
	log.Printf("%s: web certificate updated", Session(r).User)
	w.Header().Set("HX-Refresh", "true")
}

func FormFile(r *http.Request, name string) ([]byte, error) {
	f, _, err := r.FormFile(name)
	if err != nil {
		return nil, fmt.Errorf("choose a %s file", name)
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
