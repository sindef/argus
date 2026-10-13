package auth

import (
	"argus/internal/store"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

type Group struct {
	Name    string   `json:"name"`
	Role    Role     `json:"role"`
	Scopes  []string `json:"scopes,omitempty"`
	Members []string `json:"members,omitempty"`
	SSO     []string `json:"sso,omitempty"`
}

const groupsFile = "groups.json"

var validGroup = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

func (s *Store) groupsKey() string { return s.key + "-groups" }

func (s *Store) loadGroups(ctx context.Context) ([]Group, string, error) {
	var gs []Group
	rv, err := store.LoadJSON(ctx, s.secrets, s.groupsKey(), groupsFile, &gs)
	return gs, rv, err
}

func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	gs, _, err := s.loadGroups(ctx)
	return gs, err
}

func (s *Store) modifyGroups(ctx context.Context, fn func([]Group) ([]Group, error)) error {
	for attempt := 0; ; attempt++ {
		gs, rv, err := s.loadGroups(ctx)
		if err != nil {
			return err
		}
		before := slices.Clone(gs)
		for i := range before {
			before[i].Members = slices.Clone(before[i].Members)
		}
		if gs, err = fn(gs); err != nil {
			return err
		}
		err = store.SaveJSON(ctx, s.secrets, s.groupsKey(), groupsFile, rv, gs)
		if errors.Is(err, store.ErrConflict) && attempt < 3 {
			continue
		}
		if err == nil {
			s.refreshGroupSessions(ctx, before, gs)
		}
		return err
	}
}

func (s *Store) refreshGroupSessions(ctx context.Context, before, after []Group) {
	changed := map[string]bool{}
	for _, g := range append(slices.Clone(before), after...) {
		b := slices.IndexFunc(before, func(x Group) bool { return x.Name == g.Name })
		a := slices.IndexFunc(after, func(x Group) bool { return x.Name == g.Name })
		if b >= 0 && a >= 0 && before[b].Role == after[a].Role && slices.Equal(before[b].Scopes, after[a].Scopes) && slices.Equal(before[b].Members, after[a].Members) {
			continue
		}
		changed[g.Name] = true
	}
	s.refreshSessions(ctx, func(u User) bool {
		hit := slices.ContainsFunc(u.Groups, func(n string) bool { return changed[n] })
		for _, g := range append(slices.Clone(before), after...) {
			hit = hit || changed[g.Name] && slices.Contains(g.Members, u.Name)
		}
		return hit
	})
}

func (s *Store) refreshSessions(ctx context.Context, hit func(User) bool) {
	us, _ := s.Users(ctx)
	for _, u := range us {
		if !hit(u) {
			continue
		}
		if u.SSO {
			s.dropSessions(u.Name)
			continue
		}
		role, scopes := s.Access(ctx, u)
		s.sessions.mu.Lock()
		for k, v := range s.sessions.m {
			if v.Org == s.org && v.User == u.Name {
				v.Role, v.Scopes = role, scopes
				s.sessions.m[k] = v
				s.sessions.dirty = true
			}
		}
		s.sessions.mu.Unlock()
	}
}

func (s *Store) SaveGroup(ctx context.Context, old string, g Group) error {
	g.Name = strings.TrimSpace(g.Name)
	if !validGroup.MatchString(g.Name) {
		return errors.New("group name must be letters, digits, spaces, dots, dashes or underscores")
	}
	if !s.ValidRole(ctx, g.Role) {
		return errors.New("invalid role")
	}
	if err := ValidScopes(g.Scopes); err != nil {
		return err
	}
	us, err := s.Users(ctx)
	if err != nil {
		return err
	}
	for _, m := range g.Members {
		if !slices.ContainsFunc(us, func(u User) bool { return u.Name == m && !u.SSO }) {
			return fmt.Errorf("%s is not a local user", m)
		}
	}
	return s.modifyGroups(ctx, func(gs []Group) ([]Group, error) {
		i := slices.IndexFunc(gs, func(x Group) bool { return x.Name == old })
		if old != "" && i < 0 {
			return nil, fmt.Errorf("group %s not found", old)
		}
		if g.Name != old && slices.ContainsFunc(gs, func(x Group) bool { return x.Name == g.Name }) {
			return nil, fmt.Errorf("group %s already exists", g.Name)
		}
		if i >= 0 {
			gs[i] = g
			return gs, nil
		}
		return append(gs, g), nil
	})
}

func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	return s.modifyGroups(ctx, func(gs []Group) ([]Group, error) {
		i := slices.IndexFunc(gs, func(x Group) bool { return x.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("group %s not found", name)
		}
		return slices.Delete(gs, i, i+1), nil
	})
}

func (s *Store) SetMembership(ctx context.Context, user string, groups []string) error {
	return s.modifyGroups(ctx, func(gs []Group) ([]Group, error) {
		for i := range gs {
			gs[i].Members = slices.DeleteFunc(gs[i].Members, func(m string) bool { return m == user })
			if slices.Contains(groups, gs[i].Name) {
				gs[i].Members = append(gs[i].Members, user)
			}
		}
		return gs, nil
	})
}

type grant struct {
	role   Role
	scopes []string
}

func combine(gs []grant) (Role, []string, bool) {
	var best Role
	for _, g := range gs {
		if best == "" || g.role.Can(best) {
			best = g.role
		}
	}
	if best == "" {
		return "", nil, false
	}
	var scopes []string
	for _, g := range gs {
		if g.role != best {
			continue
		}
		if len(g.scopes) == 0 {
			return best, nil, true
		}
		for _, sc := range g.scopes {
			if !slices.Contains(scopes, sc) {
				scopes = append(scopes, sc)
			}
		}
	}
	return best, scopes, true
}

func (s *Store) Access(ctx context.Context, u User) (Role, []string) {
	if u.SSO {
		return u.Role, u.Scopes
	}
	gs, _ := s.Groups(ctx)
	custom, _ := s.Roles(ctx)
	grants := []grant{{tier(u.Role, custom), u.Scopes}}
	for _, g := range gs {
		if slices.Contains(g.Members, u.Name) {
			grants = append(grants, grant{tier(g.Role, custom), g.Scopes})
		}
	}
	role, scopes, _ := combine(grants)
	return role, scopes
}

func (s *Store) SSOAccess(ctx context.Context, claimed []string) (Role, []string, []string, bool) {
	gs, err := s.Groups(ctx)
	if err != nil {
		return "", nil, nil, false
	}
	var grants []grant
	var matched []string
	custom, _ := s.Roles(ctx)
	for _, g := range gs {
		g.Role = tier(g.Role, custom)
		if slices.ContainsFunc(g.SSO, func(n string) bool {
			return slices.ContainsFunc(claimed, func(c string) bool { return strings.EqualFold(c, n) })
		}) {
			grants = append(grants, grant{g.Role, g.Scopes})
			matched = append(matched, g.Name)
		}
	}
	role, scopes, ok := combine(grants)
	return role, scopes, matched, ok
}
