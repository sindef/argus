package ui

import (
	"net/http"

	"argus/web"

	"argus/internal/auth"
)

const (
	Public   auth.Privilege = ""
	SignedIn auth.Privilege = "signed-in"
	OrgAdmin auth.Privilege = "org-admin"
)

type Route struct {
	Pattern string
	Priv    auth.Privilege
	H       http.HandlerFunc
}

func (s *Core) Guard(p auth.Privilege, h http.HandlerFunc) http.HandlerFunc {
	switch p {
	case Public:
		return h
	case SignedIn:
		return s.Authenticate(h)
	case OrgAdmin:
		return s.RequireOrg(h)
	}
	return s.Need(p, h)
}

func (c *Core) Routes() []Route {
	return []Route{
		{"GET /static/", Public, http.FileServerFS(web.FS).ServeHTTP},
		{"GET /audit", OrgAdmin, c.auditPage},
		{"POST /certificate", OrgAdmin, c.setCertificate},
		{"POST /notify", OrgAdmin, c.notifyForm},
		{"GET /login", Public, c.loginPage},
		{"POST /login", Public, c.login},
		{"POST /login/totp", Public, c.loginTOTP},
		{"POST /account/mfa/{op}", SignedIn, c.mfaAction},
		{"POST /users/{name}/mfa-reset", OrgAdmin, c.resetMFA},
		{"POST /logout", Public, c.logout},
		{"GET /password", SignedIn, c.passwordPage},
		{"POST /password", SignedIn, c.changePassword},
		{"GET /users", OrgAdmin, c.usersPage},
		{"POST /users", OrgAdmin, c.addUser},
		{"POST /users/{name}/role", OrgAdmin, c.setRole},
		{"POST /users/scopes", OrgAdmin, c.setScopes},
		{"POST /users/{name}/update", OrgAdmin, c.updateUser},
		{"POST /users/{name}/reset", OrgAdmin, c.resetPassword},
		{"POST /users/{name}/delete", OrgAdmin, c.deleteUser},
	}
}
