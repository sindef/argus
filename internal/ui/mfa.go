package ui

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

func (s *Core) mfaStart(org, user string, remember bool) string {
	tok := NewToken()
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

func (s *Core) mfaAccept(org, user string, counter int64) bool {
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

func (s *Core) StoreFor(org string) *auth.Store {
	if s.MultiOrg {
		return s.Auth.ForOrg(org)
	}
	return s.Auth
}

func (s *Core) loginTOTP(w http.ResponseWriter, r *http.Request) {
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
		s.Render(w, r, "login.html", "auth", s.LoginView(r, "Sign in again"))
		return
	}
	ip, acct := ClientIP(r), accountKey(p.org, p.user)
	store := s.StoreFor(p.org)
	us, _ := store.Users(r.Context())
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == p.user })
	var counter int64
	valid := false
	if i >= 0 {
		counter, valid = auth.CheckTOTP(us[i].TOTP, r.FormValue("code"), time.Now())
	}
	if !valid || !s.mfaAccept(p.org, p.user, counter) {
		s.Limiter.Fail(ip)
		s.Limiter.Fail(acct)
		s.Record(s.AuditUser(p.org, p.user), "two-factor code from "+ip, false, "invalid code")
		w.WriteHeader(http.StatusUnauthorized)
		data := s.LoginView(r, "Invalid code")
		data["Pending"] = pend
		s.Render(w, r, "login.html", "auth", data)
		return
	}
	s.mfa.mu.Lock()
	delete(s.mfa.pending, pend)
	s.mfa.mu.Unlock()
	s.Limiter.Succeed(ip)
	s.Limiter.Succeed(acct)
	s.Record(s.AuditUser(p.org, p.user), "sign in from "+ip, true, "")
	s.SetSessionCookie(w, store.StartSession(r.Context(), us[i], p.remember), p.remember)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Core) mfaUser(r *http.Request) (auth.User, error) {
	us, err := s.Users(r).Users(r.Context())
	if err != nil {
		return auth.User{}, err
	}
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == Session(r).User })
	if i < 0 {
		return auth.User{}, errors.New("user not found")
	}
	return us[i], nil
}

func (s *Core) passwordData(r *http.Request, msg string) map[string]any {
	u, _ := s.mfaUser(r)
	data := map[string]any{"Error": msg, "MFA": u.TOTP != "", "SSO": u.SSO}
	if u.TOTP == "" && !u.SSO && r.URL.Query().Get("mfa") == "setup" {
		secret := auth.NewTOTPSecret()
		s.mfa.mu.Lock()
		if s.mfa.setup == nil {
			s.mfa.setup = map[string]string{}
		}
		s.mfa.setup[Session(r).Token] = secret
		s.mfa.mu.Unlock()
		code, err := qr.Encode(auth.TOTPURI(s.Product, Session(r).User, secret), qr.M)
		if err == nil {
			data["QR"] = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
		}
		data["Secret"] = secret
	}
	return data
}

func (s *Core) mfaAction(w http.ResponseWriter, r *http.Request) {
	sess := Session(r)
	u, err := s.mfaUser(r)
	if err == nil {
		switch r.PathValue("op") {
		case "enable":
			s.mfa.mu.Lock()
			secret := s.mfa.setup[Session(r).Token]
			s.mfa.mu.Unlock()
			counter, ok := auth.CheckTOTP(secret, r.FormValue("code"), time.Now())
			switch {
			case u.TOTP != "" || secret == "":
				err = errors.New("start setup again")
			case !ok || !s.mfaAccept(sess.Org, sess.User, counter):
				err = errors.New("invalid code; check the time on your device")
			default:
				err = s.Users(r).SetTOTP(r.Context(), sess.User, secret)
			}
			if err == nil {
				s.mfa.mu.Lock()
				delete(s.mfa.setup, Session(r).Token)
				s.mfa.mu.Unlock()
			}
		case "disable":
			if _, ok, _ := s.Users(r).Verify(r.Context(), sess.User, r.FormValue("password")); !ok {
				err = errors.New("wrong password")
			} else {
				err = s.Users(r).SetTOTP(r.Context(), sess.User, "")
			}
		default:
			err = errors.New("unknown operation")
		}
	}
	if err != nil {
		Failed(w, r, err)
		s.Render(w, r, "password.html", "layout", s.passwordData(r, err.Error()))
		return
	}
	log.Printf("%s: two-factor %s", sess.User, r.PathValue("op"))
	s.Record(s.AuditUser(sess.Org, sess.User), "two-factor "+r.PathValue("op"), true, "")
	http.Redirect(w, r, "/password", http.StatusSeeOther)
}

func (s *Core) resetMFA(w http.ResponseWriter, r *http.Request) {
	if err := s.Users(r).SetTOTP(r.Context(), r.PathValue("name"), ""); err != nil {
		Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}
