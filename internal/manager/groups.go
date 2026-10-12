package manager

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"

	"argus/internal/auth"
)

type Group struct {
	Datacenter string                     `json:"datacenter"`
	Clusters   []string                   `json:"clusters"`
	Profiles   map[string]Profile         `json:"profiles,omitempty"`
	Settings   map[string]ClusterSettings `json:"settings,omitempty"`
}

type ClusterSettings struct {
	CPUModel  string `json:"cpuModel,omitempty"`
	DRS       string `json:"drs,omitempty"`
	Threshold int    `json:"threshold,omitempty"`
	Rules     []Rule `json:"rules,omitempty"`
}

type Profile struct {
	DNS             []string `json:"dns,omitempty"`
	NTP             []string `json:"ntp,omitempty"`
	Syslog          string   `json:"syslog,omitempty"`
	MigrationBridge string   `json:"migrationBridge,omitempty"`
	Backup          string   `json:"backup,omitempty"`
	Networks        bool     `json:"networks,omitempty"`
}

var validGroup = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,39}$`)

func checkGroupName(name string) error {
	if !validGroup.MatchString(name) || name == "Unassigned" {
		return errors.New("names are letters, digits, spaces, dots, dashes or underscores, up to 40 characters")
	}
	return nil
}

func groupsKey(org string) string { return "argus-groups-" + org }

func (c *Manager) Groups(ctx context.Context, org string) ([]Group, error) {
	gs, _, err := c.loadGroups(ctx, org)
	return gs, err
}

func (c *Manager) loadGroups(ctx context.Context, org string) ([]Group, string, error) {
	var gs []Group
	rv, err := auth.LoadJSON(ctx, c.store, groupsKey(org), "groups", &gs)
	return gs, rv, err
}

func (c *Manager) updateGroups(ctx context.Context, org string, fn func([]Group, []Node) ([]Group, error)) error {
	c.haMu.Lock()
	defer c.haMu.Unlock()
	gs, rv, err := c.loadGroups(ctx, org)
	if err != nil {
		return err
	}
	nodes, nrv, err := c.load(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.Org == org && n.Datacenter != "" {
			gs = addGroup(gs, n.Datacenter, n.Cluster)
		}
	}
	before := slices.Clone(nodes)
	if gs, err = fn(gs, nodes); err != nil {
		return err
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].Datacenter < gs[j].Datacenter })
	for i := range gs {
		sort.Strings(gs[i].Clusters)
	}
	if !reflect.DeepEqual(before, nodes) {
		if err := c.save(ctx, nodes, nrv); err != nil {
			return err
		}
	}
	return auth.SaveJSON(ctx, c.store, groupsKey(org), "groups", rv, gs)
}

func addGroup(gs []Group, dc, cl string) []Group {
	i := slices.IndexFunc(gs, func(g Group) bool { return g.Datacenter == dc })
	if i < 0 {
		gs = append(gs, Group{Datacenter: dc})
		i = len(gs) - 1
	}
	if cl != "" && !slices.Contains(gs[i].Clusters, cl) {
		gs[i].Clusters = append(gs[i].Clusters, cl)
	}
	return gs
}

func findDC(gs []Group, dc string) int {
	return slices.IndexFunc(gs, func(g Group) bool { return g.Datacenter == dc })
}

func (c *Manager) AddDatacenter(ctx context.Context, org, dc string) error {
	if err := checkGroupName(dc); err != nil {
		return err
	}
	return c.updateGroups(ctx, org, func(gs []Group, _ []Node) ([]Group, error) {
		if findDC(gs, dc) >= 0 {
			return nil, fmt.Errorf("datacenter %s already exists", dc)
		}
		return addGroup(gs, dc, ""), nil
	})
}

func (c *Manager) AddCluster(ctx context.Context, org, dc, cl string) error {
	if err := checkGroupName(cl); err != nil {
		return err
	}
	return c.updateGroups(ctx, org, func(gs []Group, _ []Node) ([]Group, error) {
		i := findDC(gs, dc)
		if i < 0 {
			return nil, fmt.Errorf("datacenter %s not found", dc)
		}
		if slices.Contains(gs[i].Clusters, cl) {
			return nil, fmt.Errorf("cluster %s already exists in %s", cl, dc)
		}
		return addGroup(gs, dc, cl), nil
	})
}

func (c *Manager) RenameDatacenter(ctx context.Context, org, dc, name string) error {
	if err := checkGroupName(name); err != nil {
		return err
	}
	return c.updateGroups(ctx, org, func(gs []Group, nodes []Node) ([]Group, error) {
		i := findDC(gs, dc)
		switch {
		case i < 0:
			return nil, fmt.Errorf("datacenter %s not found", dc)
		case findDC(gs, name) >= 0:
			return nil, fmt.Errorf("datacenter %s already exists", name)
		}
		gs[i].Datacenter = name
		for j := range nodes {
			if nodes[j].Org == org && nodes[j].Datacenter == dc {
				nodes[j].Datacenter = name
			}
		}
		return gs, nil
	})
}

func (c *Manager) RenameCluster(ctx context.Context, org, dc, cl, name string) error {
	if err := checkGroupName(name); err != nil {
		return err
	}
	return c.updateGroups(ctx, org, func(gs []Group, nodes []Node) ([]Group, error) {
		i := findDC(gs, dc)
		if i < 0 || !slices.Contains(gs[i].Clusters, cl) {
			return nil, fmt.Errorf("cluster %s not found in %s", cl, dc)
		}
		if slices.Contains(gs[i].Clusters, name) {
			return nil, fmt.Errorf("cluster %s already exists in %s", name, dc)
		}
		gs[i].Clusters[slices.Index(gs[i].Clusters, cl)] = name
		if p, ok := gs[i].Profiles[cl]; ok {
			delete(gs[i].Profiles, cl)
			gs[i].Profiles[name] = p
		}
		if st, ok := gs[i].Settings[cl]; ok {
			delete(gs[i].Settings, cl)
			gs[i].Settings[name] = st
		}
		for j := range nodes {
			if nodes[j].Org == org && nodes[j].Datacenter == dc && nodes[j].Cluster == cl {
				nodes[j].Cluster = name
			}
		}
		return gs, nil
	})
}

func (c *Manager) DeleteGroup(ctx context.Context, org, dc, cl string) error {
	return c.updateGroups(ctx, org, func(gs []Group, nodes []Node) ([]Group, error) {
		for _, n := range nodes {
			if n.Org == org && n.Datacenter == dc && (cl == "" || n.Cluster == cl) {
				return nil, fmt.Errorf("%s still holds host %s; move or remove it first", map[bool]string{true: dc, false: cl}[cl == ""], n.Name)
			}
		}
		i := findDC(gs, dc)
		if i < 0 {
			return nil, fmt.Errorf("datacenter %s not found", dc)
		}
		if cl == "" {
			return slices.Delete(gs, i, i+1), nil
		}
		gs[i].Clusters = slices.DeleteFunc(gs[i].Clusters, func(x string) bool { return x == cl })
		delete(gs[i].Profiles, cl)
		delete(gs[i].Settings, cl)
		return gs, nil
	})
}

func (c *Manager) MoveNode(ctx context.Context, org, name, dc, cl string) error {
	return c.updateGroups(ctx, org, func(gs []Group, nodes []Node) ([]Group, error) {
		if dc == "" && cl != "" {
			return nil, errors.New("a cluster needs a datacenter")
		}
		for _, x := range []string{dc, cl} {
			if x != "" {
				if err := checkGroupName(x); err != nil {
					return nil, err
				}
			}
		}
		for j := range nodes {
			if nodes[j].Org == org && nodes[j].Name == name {
				nodes[j].Datacenter, nodes[j].Cluster = dc, cl
				if dc != "" {
					gs = addGroup(gs, dc, cl)
				}
				return gs, nil
			}
		}
		return nil, fmt.Errorf("host %s not found", name)
	})
}

func (c *Manager) Profile(ctx context.Context, org, dc, cl string) (Profile, error) {
	gs, err := c.Groups(ctx, org)
	if err != nil {
		return Profile{}, err
	}
	if i := findDC(gs, dc); i >= 0 {
		return gs[i].Profiles[cl], nil
	}
	return Profile{}, nil
}

func (c *Manager) SetProfile(ctx context.Context, org, dc, cl string, p Profile) error {
	return c.updateGroups(ctx, org, func(gs []Group, _ []Node) ([]Group, error) {
		i := findDC(gs, dc)
		if i < 0 || !slices.Contains(gs[i].Clusters, cl) {
			return nil, fmt.Errorf("cluster %s not found in %s", cl, dc)
		}
		if gs[i].Profiles == nil {
			gs[i].Profiles = map[string]Profile{}
		}
		gs[i].Profiles[cl] = p
		return gs, nil
	})
}

func (c *Manager) ClusterSettings(ctx context.Context, org, dc, cl string) (ClusterSettings, error) {
	gs, err := c.Groups(ctx, org)
	if err != nil {
		return ClusterSettings{}, err
	}
	if i := findDC(gs, dc); i >= 0 {
		return gs[i].Settings[cl], nil
	}
	return ClusterSettings{}, nil
}

func (c *Manager) SetClusterSettings(ctx context.Context, org, dc, cl string, st ClusterSettings) error {
	return c.updateGroups(ctx, org, func(gs []Group, _ []Node) ([]Group, error) {
		i := findDC(gs, dc)
		if i < 0 || !slices.Contains(gs[i].Clusters, cl) {
			return nil, fmt.Errorf("cluster %s not found in %s", cl, dc)
		}
		if gs[i].Settings == nil {
			gs[i].Settings = map[string]ClusterSettings{}
		}
		gs[i].Settings[cl] = st
		return gs, nil
	})
}

func (c *Manager) CommonCPUModels(org, dc, cl string) []string {
	var common []string
	first := true
	for _, st := range c.Inventory(org) {
		if st.Node.Datacenter != dc || st.Node.Cluster != cl || !st.Reachable() {
			continue
		}
		if first {
			common, first = slices.Clone(st.Info.CPU.Models), false
			continue
		}
		common = slices.DeleteFunc(common, func(m string) bool { return !slices.Contains(st.Info.CPU.Models, m) })
	}
	return common
}
