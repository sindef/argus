package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"argus/internal/auth"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const ssoCookie = "argus_sso_flow"

type ssoFlow struct {
	binding  string
	org      string
	verifier string
	nonce    string
	remember bool
	expires  time.Time
}

type ssoFlows struct {
	mu sync.Mutex
	m  map[string]ssoFlow
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) oauthConfig(r *http.Request, org string) (*oauth2.Config, *oidc.Provider, *oidcSettings, error) {
	o, err := s.manager.Org(r.Context(), org)
	if err != nil || !o.SSO() {
		return nil, nil, nil, errors.New("single sign-on is not configured for this organization")
	}
	provider, err := oidc.NewProvider(r.Context(), o.OIDC.Issuer)
	if err != nil {
		return nil, nil, nil, err
	}
	conf := &oauth2.Config{
		ClientID:     o.OIDC.ClientID,
		ClientSecret: o.OIDC.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  "https://" + r.Host + "/sso/callback",
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	claim := o.OIDC.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	return conf, provider, &oidcSettings{groupsClaim: claim, roles: o.OIDC.Roles}, nil
}

type oidcSettings struct {
	groupsClaim string
	roles       map[string]auth.Role
}

func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	org := r.FormValue("org")
	conf, _, _, err := s.oauthConfig(r, org)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "login.html", "auth", s.loginData(r, err.Error()))
		return
	}
	state, flow := randomHex(16), ssoFlow{binding: randomHex(16), org: org, verifier: oauth2.GenerateVerifier(), nonce: randomHex(16), remember: r.FormValue("remember") != "", expires: time.Now().Add(10 * time.Minute)}
	s.sso.mu.Lock()
	for k, v := range s.sso.m {
		if time.Now().After(v.expires) {
			delete(s.sso.m, k)
		}
	}
	s.sso.m[state] = flow
	s.sso.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: flow.binding, Path: "/sso/callback", MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, conf.AuthCodeURL(state, oidc.Nonce(flow.nonce), oauth2.S256ChallengeOption(flow.verifier)), http.StatusFound)
}

func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string, err error) {
		log.Printf("sso: %s: %v", msg, err)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", "auth", s.loginData(r, msg))
	}
	state := r.URL.Query().Get("state")
	s.sso.mu.Lock()
	flow, ok := s.sso.m[state]
	delete(s.sso.m, state)
	s.sso.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/sso/callback", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	if !ok || time.Now().After(flow.expires) {
		fail("Sign-in expired, try again", errors.New("unknown or expired state"))
		return
	}
	if c, err := r.Cookie(ssoCookie); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(flow.binding)) != 1 {
		fail("Sign-in must finish in the browser that started it", errors.New("flow cookie missing or mismatched"))
		return
	}
	conf, provider, settings, err := s.oauthConfig(r, flow.org)
	if err != nil {
		fail("Single sign-on failed", err)
		return
	}
	tok, err := conf.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(flow.verifier))
	if err != nil {
		fail("Single sign-on failed", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := provider.Verifier(&oidc.Config{ClientID: conf.ClientID}).Verify(r.Context(), raw)
	if err != nil || idt.Nonce != flow.nonce {
		fail("Single sign-on failed", err)
		return
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		fail("Single sign-on failed", err)
		return
	}
	user := firstString(claims, "preferred_username", "email", "sub")
	if user == "" {
		fail("Single sign-on failed", errors.New("no username claim"))
		return
	}
	role, ok := mapRole(claims[settings.groupsClaim], settings.roles)
	if !ok {
		fail("Your account has no access to this organization", errors.New(user+": no mapped group"))
		return
	}
	if err := s.auth.ForOrg(flow.org).UpsertSSO(r.Context(), user, idt.Issuer+"|"+idt.Subject, role); err != nil {
		fail("Single sign-on failed", err)
		return
	}
	session := s.auth.ForOrg(flow.org).NewSession(auth.Session{User: user, Role: role, SSO: true}, flow.remember)
	s.setSessionCookie(w, session, flow.remember)
	log.Printf("sso: %s signed in to %s as %s", user, flow.org, role)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func firstString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := claims[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func mapRole(groups any, roles map[string]auth.Role) (auth.Role, bool) {
	list, _ := groups.([]any)
	var best auth.Role
	found := false
	for _, g := range list {
		name, _ := g.(string)
		if r, ok := roles[name]; ok && (!found || r.Can(best)) {
			best, found = r, true
		}
	}
	return best, found
}
