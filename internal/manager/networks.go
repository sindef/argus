package manager

import (
	"context"
	"fmt"
	"sort"

	"argus/internal/auth"
)

type Network struct {
	Name        string   `json:"name"`
	VLAN        int      `json:"vlan"`
	CIDR        string   `json:"cidr"`
	Gateway     string   `json:"gateway"`
	Description string   `json:"description"`
	Bridge      string   `json:"bridge,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	SpoofCheck  bool     `json:"spoofCheck,omitempty"`
	RateMbps    int      `json:"rateMbps,omitempty"`
}

func (n Network) BridgeName() string {
	if n.Bridge == "" {
		return "br0"
	}
	return n.Bridge
}

func networksKey(org string) string { return "argus-networks-" + org }

func (m *Manager) Networks(ctx context.Context, org string) ([]Network, error) {
	ns, _, err := m.loadNetworks(ctx, org)
	return ns, err
}

func (m *Manager) loadNetworks(ctx context.Context, org string) ([]Network, string, error) {
	var ns []Network
	rv, err := auth.LoadJSON(ctx, m.store, networksKey(org), "networks", &ns)
	return ns, rv, err
}

func (m *Manager) updateNetworks(ctx context.Context, org string, fn func([]Network) ([]Network, error)) error {
	m.haMu.Lock()
	defer m.haMu.Unlock()
	ns, rv, err := m.loadNetworks(ctx, org)
	if err != nil {
		return err
	}
	if ns, err = fn(ns); err != nil {
		return err
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	return auth.SaveJSON(ctx, m.store, networksKey(org), "networks", rv, ns)
}

func (m *Manager) SaveNetwork(ctx context.Context, org string, n Network, replace bool) error {
	return m.updateNetworks(ctx, org, func(ns []Network) ([]Network, error) {
		for i, x := range ns {
			if x.Name == n.Name && replace {
				ns[i] = n
				return ns, nil
			}
			if x.Name == n.Name {
				return nil, fmt.Errorf("network %s already exists", n.Name)
			}
		}
		if replace {
			return nil, fmt.Errorf("network %s not found", n.Name)
		}
		return append(ns, n), nil
	})
}

func (m *Manager) DeleteNetwork(ctx context.Context, org, name string) error {
	return m.updateNetworks(ctx, org, func(ns []Network) ([]Network, error) {
		out := ns[:0]
		for _, x := range ns {
			if x.Name != name {
				out = append(out, x)
			}
		}
		return out, nil
	})
}
