package manager

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

type LDAP struct {
	URL          string `json:"url"`
	StartTLS     bool   `json:"startTLS,omitempty"`
	CA           string `json:"ca,omitempty"`
	BindDN       string `json:"bindDN"`
	BindPassword string `json:"bindPassword"`
	BaseDN       string `json:"baseDN"`
	UserFilter   string `json:"userFilter,omitempty"`
	GroupFilter  string `json:"groupFilter,omitempty"`
}

const (
	defaultUserFilter  = "(|(sAMAccountName={user})(uid={user})(userPrincipalName={user}))"
	defaultGroupFilter = "(|(member={dn})(uniqueMember={dn})(memberUid={user}))"
)

func (l LDAP) dial() (*ldap.Conn, error) {
	u, err := url.Parse(l.URL)
	if err != nil || (u.Scheme != "ldaps" && u.Scheme != "ldap") || u.Host == "" {
		return nil, errors.New("directory URL must be ldaps://host or ldap://host")
	}
	if u.Scheme == "ldap" && !l.StartTLS {
		return nil, errors.New("plain ldap:// needs StartTLS")
	}
	cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if l.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(l.CA)) {
			return nil, errors.New("CA certificate is not valid PEM")
		}
		cfg.RootCAs = pool
	}
	c, err := ldap.DialURL(l.URL, ldap.DialWithTLSConfig(cfg))
	if err != nil {
		return nil, err
	}
	c.SetTimeout(15 * time.Second)
	if l.StartTLS {
		if err := c.StartTLS(cfg); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

func (l LDAP) Test() error {
	c, err := l.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Bind(l.BindDN, l.BindPassword)
}

func cnOf(dn string) string {
	if p, err := ldap.ParseDN(dn); err == nil && len(p.RDNs) > 0 {
		for _, a := range p.RDNs[0].Attributes {
			if strings.EqualFold(a.Type, "cn") {
				return a.Value
			}
		}
	}
	return ""
}

func (l LDAP) Authenticate(ctx context.Context, user, password string) (string, []string, error) {
	if user == "" || password == "" {
		return "", nil, errors.New("empty credentials")
	}
	c, err := l.dial()
	if err != nil {
		return "", nil, err
	}
	defer c.Close()
	if err := c.Bind(l.BindDN, l.BindPassword); err != nil {
		return "", nil, fmt.Errorf("service bind: %w", err)
	}
	filter := strings.ReplaceAll(cmp.Or(l.UserFilter, defaultUserFilter), "{user}", ldap.EscapeFilter(user))
	res, err := c.Search(ldap.NewSearchRequest(l.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 15, false, filter, []string{"memberOf"}, nil))
	if err != nil {
		return "", nil, err
	}
	if len(res.Entries) != 1 {
		return "", nil, errors.New("user not found")
	}
	dn := res.Entries[0].DN
	groups := map[string]bool{}
	for _, g := range res.Entries[0].GetAttributeValues("memberOf") {
		groups[g], groups[cnOf(g)] = true, true
	}
	gfilter := strings.NewReplacer("{dn}", ldap.EscapeFilter(dn), "{user}", ldap.EscapeFilter(user)).Replace(cmp.Or(l.GroupFilter, defaultGroupFilter))
	if gres, err := c.Search(ldap.NewSearchRequest(l.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1000, 15, false, gfilter, []string{"cn"}, nil)); err == nil {
		for _, e := range gres.Entries {
			groups[e.DN], groups[e.GetAttributeValue("cn")] = true, true
		}
	}
	if err := c.Bind(dn, password); err != nil {
		return "", nil, errors.New("invalid password")
	}
	delete(groups, "")
	var out []string
	for g := range groups {
		out = append(out, g)
	}
	return dn, out, nil
}

func (o Org) Directory() bool { return o.LDAP != nil && o.LDAP.URL != "" }
