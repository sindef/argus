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
	Stream  bool
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
		{Pattern: "GET /static/", Priv: Public, H: http.FileServerFS(web.FS).ServeHTTP},
		{Pattern: "GET /audit", Priv: OrgAdmin, H: c.auditPage},
		{Pattern: "POST /certificate", Priv: OrgAdmin, H: c.setCertificate},
		{Pattern: "POST /notify", Priv: OrgAdmin, H: c.notifyForm},
		{Pattern: "POST /alarms/ack", Priv: auth.Administer, H: c.ackAlarm},
		{Pattern: "GET /tasks", Priv: SignedIn, H: c.tasksBar},
		{Pattern: "POST /tasks/dismiss", Priv: SignedIn, H: c.dismissTask},
		{Pattern: "GET /login", Priv: Public, H: c.loginPage},
		{Pattern: "POST /login", Priv: Public, H: c.login},
		{Pattern: "POST /login/totp", Priv: Public, H: c.loginTOTP},
		{Pattern: "POST /account/mfa/{op}", Priv: SignedIn, H: c.mfaAction},
		{Pattern: "POST /users/{name}/mfa-reset", Priv: OrgAdmin, H: c.resetMFA},
		{Pattern: "POST /logout", Priv: Public, H: c.logout},
		{Pattern: "GET /password", Priv: SignedIn, H: c.passwordPage},
		{Pattern: "POST /password", Priv: SignedIn, H: c.changePassword},
		{Pattern: "GET /users", Priv: OrgAdmin, H: c.usersPage},
		{Pattern: "POST /users", Priv: OrgAdmin, H: c.addUser},
		{Pattern: "POST /users/{name}/role", Priv: OrgAdmin, H: c.setRole},
		{Pattern: "POST /users/scopes", Priv: OrgAdmin, H: c.setScopes},
		{Pattern: "POST /users/{name}/update", Priv: OrgAdmin, H: c.updateUser},
		{Pattern: "POST /users/{name}/reset", Priv: OrgAdmin, H: c.resetPassword},
		{Pattern: "POST /users/{name}/delete", Priv: OrgAdmin, H: c.deleteUser},
	}
}
