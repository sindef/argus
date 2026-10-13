package ui

import (
	"log"
	"net/http"
	"strings"
	"time"
)

func (s *Core) sessionValid(r *http.Request) bool {
	if tok := Session(r).Token; tok != "" {
		_, ok := s.Auth.Session(tok)
		return ok
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		t, valid := s.Auth.CheckToken(r.Context(), tok)
		return valid && t.Scope == ""
	}
	return false
}

func (s *Core) WaitConsole(r *http.Request, done <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if !s.sessionValid(r) {
				log.Printf("%s: console closed, session revoked", Session(r).User)
				return
			}
		}
	}
}
