package server

import (
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"argus/internal/auth"

	"rsc.io/qr"
)

const mfaWindow = 5 * time.Minute

type mfaPending struct {
	org, user string
	remember  bool
	expires   time.Time
	tries     int
}

type mfaState struct {
	mu      sync.Mutex
	pending map[string]mfaPending
	used    map[string]int64
	setup   map[string]string
}

func (s *Server) mfaStart(org, user string, remember bool) string {
	tok := newToken()
	s.mfa.mu.Lock()
	defer s.mfa.mu.Unlock()
	if s.mfa.pending == nil {
		s.mfa.pending = map[string]mfaPending{}
	}
	for k, p := range s.mfa.pending {
		if time.Now().After(p.expires) {
			delete(s.mfa.pending, k)
		}
	}
	s.mfa.pending[tok] = mfaPending{org: org, user: user, remember: remember, expires: time.Now().Add(mfaWindow)}
	return tok
}

func (s *Server) mfaAccept(org, user string, counter int64) bool {
	s.mfa.mu.Lock()
	defer s.mfa.mu.Unlock()
	if s.mfa.used == nil {
		s.mfa.used = map[string]int64{}
	}
	k := org + "/" + user
	if counter <= s.mfa.used[k] {
		return false
	}
	s.mfa.used[k] = counter
	return true
}

func (s *Server) storeFor(org string) *auth.Store {
	if s.manager != nil {
		return s.auth.ForOrg(org)
	}
	return s.auth
}

func (s *Server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	pend := r.FormValue("pending")
	s.mfa.mu.Lock()
	p, ok := s.mfa.pending[pend]
	if ok {
		p.tries++
		s.mfa.pending[pend] = p
	}
	s.mfa.mu.Unlock()
	if !ok || time.Now().After(p.expires) || p.tries > 5 {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", "auth", s.loginData(r, "Sign in again"))
		return
	}
	ip, acct := clientIP(r), accountKey(p.org, p.user)
	store := s.storeFor(p.org)
	us, _ := store.Users(r.Context())
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == p.user })
	var counter int64
	valid := false
	if i >= 0 {
		counter, valid = auth.CheckTOTP(us[i].TOTP, r.FormValue("code"), time.Now())
	}
	if !valid || !s.mfaAccept(p.org, p.user, counter) {
		s.limiter.fail(ip)
		s.limiter.fail(acct)
		s.record(s.auditUser(p.org, p.user), "two-factor code from "+ip, false, "invalid code")
		w.WriteHeader(http.StatusUnauthorized)
		data := s.loginData(r, "Invalid code")
		data["Pending"] = pend
		s.render(w, r, "login.html", "auth", data)
		return
	}
	s.mfa.mu.Lock()
	delete(s.mfa.pending, pend)
	s.mfa.mu.Unlock()
	s.limiter.succeed(ip)
	s.limiter.succeed(acct)
	s.record(s.auditUser(p.org, p.user), "sign in from "+ip, true, "")
	s.setSessionCookie(w, store.StartSession(r.Context(), us[i], p.remember), p.remember)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) mfaUser(r *http.Request) (auth.User, error) {
	us, err := s.users(r).Users(r.Context())
	if err != nil {
		return auth.User{}, err
	}
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == session(r).User })
	if i < 0 {
		return auth.User{}, errors.New("user not found")
	}
	return us[i], nil
}

func (s *Server) passwordData(r *http.Request, msg string) map[string]any {
	u, _ := s.mfaUser(r)
	data := map[string]any{"Error": msg, "MFA": u.TOTP != "", "SSO": u.SSO}
	if u.TOTP == "" && !u.SSO && r.URL.Query().Get("mfa") == "setup" {
		secret := auth.NewTOTPSecret()
		s.mfa.mu.Lock()
		if s.mfa.setup == nil {
			s.mfa.setup = map[string]string{}
		}
		s.mfa.setup[session(r).Token] = secret
		s.mfa.mu.Unlock()
		issuer := "Argus"
		if s.manager != nil {
			issuer = "Argus Manager"
		}
		code, err := qr.Encode(auth.TOTPURI(issuer, session(r).User, secret), qr.M)
		if err == nil {
			data["QR"] = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
		}
		data["Secret"] = secret
	}
	return data
}

func (s *Server) mfaAction(w http.ResponseWriter, r *http.Request) {
	sess := session(r)
	u, err := s.mfaUser(r)
	if err == nil {
		switch r.PathValue("op") {
		case "enable":
			s.mfa.mu.Lock()
			secret := s.mfa.setup[session(r).Token]
			s.mfa.mu.Unlock()
			counter, ok := auth.CheckTOTP(secret, r.FormValue("code"), time.Now())
			switch {
			case u.TOTP != "" || secret == "":
				err = errors.New("start setup again")
			case !ok || !s.mfaAccept(sess.Org, sess.User, counter):
				err = errors.New("invalid code; check the time on your device")
			default:
				err = s.users(r).SetTOTP(r.Context(), sess.User, secret)
			}
			if err == nil {
				s.mfa.mu.Lock()
				delete(s.mfa.setup, session(r).Token)
				s.mfa.mu.Unlock()
			}
		case "disable":
			if _, ok, _ := s.users(r).Verify(r.Context(), sess.User, r.FormValue("password")); !ok {
				err = errors.New("wrong password")
			} else {
				err = s.users(r).SetTOTP(r.Context(), sess.User, "")
			}
		default:
			err = errors.New("unknown operation")
		}
	}
	if err != nil {
		failed(w, r, err)
		s.render(w, r, "password.html", "layout", s.passwordData(r, err.Error()))
		return
	}
	log.Printf("%s: two-factor %s", sess.User, r.PathValue("op"))
	s.record(s.auditUser(sess.Org, sess.User), "two-factor "+r.PathValue("op"), true, "")
	http.Redirect(w, r, "/password", http.StatusSeeOther)
}

func (s *Server) resetMFA(w http.ResponseWriter, r *http.Request) {
	if err := s.users(r).SetTOTP(r.Context(), r.PathValue("name"), ""); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: reset two-factor for %s", session(r).User, r.PathValue("name"))
	w.Header().Set("HX-Refresh", "true")
}
