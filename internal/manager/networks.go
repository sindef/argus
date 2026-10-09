package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Network struct {
	Name        string `json:"name"`
	VLAN        int    `json:"vlan"`
	CIDR        string `json:"cidr"`
	Gateway     string `json:"gateway"`
	Description string `json:"description"`
}

func networksKey(org string) string { return "argus-networks-" + org }

func (m *Manager) Networks(ctx context.Context, org string) ([]Network, error) {
	ns, _, err := m.loadNetworks(ctx, org)
	return ns, err
}

func (m *Manager) loadNetworks(ctx context.Context, org string) ([]Network, string, error) {
	data, rv, err := m.store.Get(ctx, networksKey(org))
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var ns []Network
	return ns, rv, json.Unmarshal(data["networks"], &ns)
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
	b, err := json.Marshal(ns)
	if err != nil {
		return err
	}
	data := map[string][]byte{"networks": b}
	if rv == "" {
		return m.store.Create(ctx, networksKey(org), "Opaque", data)
	}
	return m.store.Update(ctx, networksKey(org), rv, data)
}

func (m *Manager) SaveNetwork(ctx context.Context, org string, n Network) error {
	return m.updateNetworks(ctx, org, func(ns []Network) ([]Network, error) {
		for _, x := range ns {
			if x.Name == n.Name {
				return nil, fmt.Errorf("network %s already exists", n.Name)
			}
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
