package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

type Privilege string

const (
	VMView        Privilege = "vm.view"
	VMPower       Privilege = "vm.power"
	VMConsole     Privilege = "vm.console"
	VMConfig      Privilege = "vm.config"
	VMProvision   Privilege = "vm.provision"
	VMDelete      Privilege = "vm.delete"
	VMSnapshot    Privilege = "vm.snapshot"
	VMBackup      Privilege = "vm.backup"
	VMMigrate     Privilege = "vm.migrate"
	HostView      Privilege = "host.view"
	HostConfig    Privilege = "host.config"
	NetworkConfig Privilege = "network.config"
	ImageConfig   Privilege = "image.config"
	Administer    Privilege = "admin"
)

var PrivilegeLabels = []struct {
	P     Privilege
	Label string
}{
	{VMView, "View VMs"}, {VMPower, "Power VMs"}, {VMConsole, "Open VM consoles"}, {VMConfig, "Edit VM hardware, disks and tags"},
	{VMProvision, "Create, clone and import VMs"}, {VMDelete, "Delete VMs"}, {VMSnapshot, "Manage snapshots"}, {VMBackup, "Back up, restore and download disks"},
	{VMMigrate, "Migrate VMs and manage HA"}, {HostView, "View hosts"}, {HostConfig, "Configure hosts, storage and host networks"},
	{NetworkConfig, "Manage networks"}, {ImageConfig, "Manage images and templates"}, {Administer, "Administer hosts, clusters and load balancing"},
}

var builtinPrivileges = map[Role][]Privilege{
	ReadOnly: {VMView, HostView},
	Operator: {VMView, HostView, VMPower, VMConsole, VMConfig, VMProvision, VMDelete, VMSnapshot, VMBackup, ImageConfig},
}

func allPrivileges() []Privilege {
	out := make([]Privilege, len(PrivilegeLabels))
	for i, p := range PrivilegeLabels {
		out[i] = p.P
	}
	return out
}

type CustomRole struct {
	Name       string      `json:"name"`
	Privileges []Privilege `json:"privileges"`
}

type Grant struct {
	Privileges []Privilege
	Scopes     []string
}

type Object struct {
	Datacenter, Cluster, Host, VM string
}

func (s *Store) rolesKey() string { return s.key + "-roles" }

func (s *Store) Roles(ctx context.Context) ([]CustomRole, error) {
	var rs []CustomRole
	_, err := LoadJSON(ctx, s.secrets, s.rolesKey(), "roles", &rs)
	return rs, err
}

func (s *Store) SaveRole(ctx context.Context, old string, role CustomRole) error {
	if !validGroup.MatchString(role.Name) || Role(role.Name).Valid() {
		return errors.New("role names are letters, digits, spaces, dots, dashes or underscores, and not a built-in role")
	}
	for _, p := range role.Privileges {
		if !slices.Contains(allPrivileges(), p) {
			return fmt.Errorf("unknown privilege %q", p)
		}
	}
	var rs []CustomRole
	rv, err := LoadJSON(ctx, s.secrets, s.rolesKey(), "roles", &rs)
	if err != nil {
		return err
	}
	if old != role.Name && slices.ContainsFunc(rs, func(r CustomRole) bool { return r.Name == role.Name }) {
		return fmt.Errorf("role %s already exists", role.Name)
	}
	rs = slices.DeleteFunc(rs, func(r CustomRole) bool { return r.Name == old || r.Name == role.Name })
	rs = append(rs, role)
	slices.SortFunc(rs, func(a, b CustomRole) int { return strings.Compare(a.Name, b.Name) })
	if err := SaveJSON(ctx, s.secrets, s.rolesKey(), "roles", rv, rs); err != nil {
		return err
	}
	if old != "" && old != role.Name {
		if err := s.renameRole(ctx, Role(old), Role(role.Name)); err != nil {
			return err
		}
	}
	gs, err := s.Groups(ctx)
	if err != nil {
		return err
	}
	s.refreshSessions(ctx, func(u User) bool {
		return u.Role == Role(role.Name) || slices.ContainsFunc(gs, func(g Group) bool {
			return g.Role == Role(role.Name) && (slices.Contains(g.Members, u.Name) || u.SSO && slices.Contains(u.Groups, g.Name))
		})
	})
	return nil
}

func (s *Store) DeleteRole(ctx context.Context, name string) error {
	us, err := s.Users(ctx)
	if err != nil {
		return err
	}
	gs, err := s.Groups(ctx)
	if err != nil {
		return err
	}
	for _, u := range us {
		if u.Role == Role(name) {
			return fmt.Errorf("user %s has role %s", u.Name, name)
		}
	}
	for _, g := range gs {
		if g.Role == Role(name) {
			return fmt.Errorf("group %s has role %s", g.Name, name)
		}
	}
	var rs []CustomRole
	rv, err := LoadJSON(ctx, s.secrets, s.rolesKey(), "roles", &rs)
	if err != nil {
		return err
	}
	return SaveJSON(ctx, s.secrets, s.rolesKey(), "roles", rv, slices.DeleteFunc(rs, func(r CustomRole) bool { return r.Name == name }))
}

func (s *Store) renameRole(ctx context.Context, old, to Role) error {
	if err := s.modify(ctx, func(us []User) ([]User, error) {
		for i := range us {
			if us[i].Role == old {
				us[i].Role = to
			}
		}
		return us, nil
	}); err != nil {
		return err
	}
	return s.modifyGroups(ctx, func(gs []Group) ([]Group, error) {
		for i := range gs {
			if gs[i].Role == old {
				gs[i].Role = to
			}
		}
		return gs, nil
	})
}

func (s *Store) ValidRole(ctx context.Context, r Role) bool {
	if r.Valid() {
		return true
	}
	rs, _ := s.Roles(ctx)
	return slices.ContainsFunc(rs, func(c CustomRole) bool { return c.Name == string(r) })
}

func privilegesOf(r Role, custom []CustomRole) []Privilege {
	if r == Admin {
		return allPrivileges()
	}
	if p, ok := builtinPrivileges[r]; ok {
		return p
	}
	if i := slices.IndexFunc(custom, func(c CustomRole) bool { return c.Name == string(r) }); i >= 0 {
		return custom[i].Privileges
	}
	return nil
}

func RoleGrants(r Role) []Grant {
	return []Grant{{Privileges: privilegesOf(r, nil)}}
}

func tier(r Role, custom []CustomRole) Role {
	if r.Valid() {
		return r
	}
	ps := privilegesOf(r, custom)
	switch {
	case slices.Contains(ps, Administer):
		return Admin
	case slices.Contains(ps, VMPower) || slices.Contains(ps, VMConfig) || slices.Contains(ps, VMProvision):
		return Operator
	}
	return ReadOnly
}

func (s *Store) Grants(ctx context.Context, user string) ([]Grant, error) {
	us, err := s.Users(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(us, func(u User) bool { return u.Name == user })
	if i < 0 {
		return nil, ErrNotFound
	}
	u := us[i]
	gs, err := s.Groups(ctx)
	if err != nil {
		return nil, err
	}
	custom, err := s.Roles(ctx)
	if err != nil {
		return nil, err
	}
	var out []Grant
	if !u.SSO {
		out = append(out, Grant{privilegesOf(u.Role, custom), u.Scopes})
	}
	for _, g := range gs {
		if slices.Contains(g.Members, u.Name) || u.SSO && slices.Contains(u.Groups, g.Name) {
			out = append(out, Grant{privilegesOf(g.Role, custom), g.Scopes})
		}
	}
	return out, nil
}

func ValidScopes(scopes []string) error {
	for _, sc := range scopes {
		var ok bool
		switch {
		case strings.HasPrefix(sc, "vm:"):
			host, vm, cut := strings.Cut(strings.TrimPrefix(sc, "vm:"), "/")
			ok = cut && host != "" && vm != "" && !strings.Contains(vm, "/")
		case strings.HasPrefix(sc, "host:"):
			ok = strings.TrimPrefix(sc, "host:") != ""
		default:
			dc, cl, cut := strings.Cut(sc, "/")
			ok = dc != "" && !strings.Contains(dc, ":") && (!cut || cl != "" && !strings.Contains(cl, "/"))
		}
		if !ok {
			return fmt.Errorf("invalid scope %q: use datacenter, datacenter/cluster, host:NAME or vm:HOST/VM", sc)
		}
	}
	return nil
}

func ScopeCovers(scope string, o Object) bool {
	switch {
	case scope == "":
		return true
	case strings.HasPrefix(scope, "vm:"):
		host, vm, _ := strings.Cut(strings.TrimPrefix(scope, "vm:"), "/")
		return o.VM != "" && host == o.Host && vm == o.VM
	case strings.HasPrefix(scope, "host:"):
		return strings.TrimPrefix(scope, "host:") == o.Host
	}
	dc, cl, _ := strings.Cut(scope, "/")
	return strings.EqualFold(dc, o.Datacenter) && (cl == "" || strings.EqualFold(cl, o.Cluster))
}

func Allows(grants []Grant, p Privilege, o Object) bool {
	for _, g := range grants {
		if !slices.Contains(g.Privileges, p) {
			continue
		}
		if len(g.Scopes) == 0 || slices.ContainsFunc(g.Scopes, func(sc string) bool { return ScopeCovers(sc, o) }) {
			return true
		}
	}
	return false
}

func AllowsAnywhere(grants []Grant, p Privilege) bool {
	return slices.ContainsFunc(grants, func(g Grant) bool { return slices.Contains(g.Privileges, p) })
}

func ReachesHost(grants []Grant, p Privilege, o Object) bool {
	for _, g := range grants {
		if !slices.Contains(g.Privileges, p) {
			continue
		}
		if len(g.Scopes) == 0 || slices.ContainsFunc(g.Scopes, func(sc string) bool {
			if host, _, ok := strings.Cut(strings.TrimPrefix(sc, "vm:"), "/"); ok && strings.HasPrefix(sc, "vm:") {
				return host == o.Host
			}
			return ScopeCovers(sc, o)
		}) {
			return true
		}
	}
	return false
}
