package manager

import (
	"argus/internal/store"
	"context"
	"errors"
	"fmt"
	"sort"
)

const (
	orgsKey    = "argus-orgs"
	DefaultOrg = "default"
)

type OIDC struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
	GroupsClaim  string `json:"groupsClaim"`
}

type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OIDC *OIDC  `json:"oidc,omitempty"`
	LDAP *LDAP  `json:"ldap,omitempty"`
}

func (o Org) SSO() bool { return o.OIDC != nil && o.OIDC.Issuer != "" }

func (c *Manager) Orgs(ctx context.Context) ([]Org, error) {
	os, _, err := c.loadOrgs(ctx)
	return os, err
}

func (c *Manager) Org(ctx context.Context, id string) (Org, error) {
	os, err := c.Orgs(ctx)
	if err != nil {
		return Org{}, err
	}
	for _, o := range os {
		if o.ID == id {
			return o, nil
		}
	}
	return Org{}, fmt.Errorf("%w: %s", ErrOrgNotFound, id)
}

var ErrOrgNotFound = errors.New("organization not found")

func (c *Manager) loadOrgs(ctx context.Context) ([]Org, string, error) {
	var os []Org
	rv, err := store.LoadJSON(ctx, c.store, orgsKey, "orgs", &os)
	return os, rv, err
}

func (c *Manager) updateOrgs(ctx context.Context, fn func([]Org) ([]Org, error)) error {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	os, rv, err := c.loadOrgs(ctx)
	if err != nil {
		return err
	}
	if os, err = fn(os); err != nil {
		return err
	}
	sort.Slice(os, func(i, j int) bool { return os[i].Name < os[j].Name })
	return store.SaveJSON(ctx, c.store, orgsKey, "orgs", rv, os)
}

func (c *Manager) EnsureDefaultOrg(ctx context.Context) error {
	return c.updateOrgs(ctx, func(os []Org) ([]Org, error) {
		for _, o := range os {
			if o.ID == DefaultOrg {
				return os, nil
			}
		}
		return append(os, Org{ID: DefaultOrg, Name: "Default"}), nil
	})
}

func (c *Manager) SaveOrg(ctx context.Context, org Org) error {
	return c.updateOrgs(ctx, func(os []Org) ([]Org, error) {
		for i := range os {
			if os[i].ID == org.ID {
				os[i] = org
				return os, nil
			}
		}
		return append(os, org), nil
	})
}

func (c *Manager) DeleteOrg(ctx context.Context, id string) error {
	if id == DefaultOrg {
		return errors.New("the default organization cannot be deleted")
	}
	nodes, err := c.NodesFor(ctx, id)
	if err != nil {
		return err
	}
	if len(nodes) > 0 {
		return errors.New("remove the organization's nodes first")
	}
	return c.updateOrgs(ctx, func(os []Org) ([]Org, error) {
		out := os[:0]
		for _, o := range os {
			if o.ID != id {
				out = append(out, o)
			}
		}
		return out, nil
	})
}
