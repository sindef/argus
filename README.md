# kubevisor

Lightweight KubeVirt hypervisor. One node = one standalone single-node k3s cluster (sqlite via kine), ESXi-style. Clustering is an optional manager on top, not a property of the nodes.

## Targets

| | Floor | Sane |
|---|---|---|
| CPU | 2 cores, VT-x/AMD-V | 4 cores |
| RAM (idle, no VMs) | ~1.5GB | 4GB+ |
| Disk | 32GB OS + ZFS pool | NVMe |
| Per-VM overhead | ~150-250MB (virt-launcher) | |

## Architecture

See [docs/architecture.drawio](docs/architecture.drawio).

### Node

| Layer | Component | Notes |
|---|---|---|
| OS | Kairos (Ubuntu 24.04 base, k3s provider), KVM, `br0`, ZFS pool | Immutable A/B image built from `os/Dockerfile`. `br0` with VLAN filtering, uplink enslaved |
| K8s | k3s server, sqlite (kine) | `--disable traefik,servicelb,metrics-server,local-storage --disable-cloud-controller --disable-network-policy --flannel-backend=host-gw` |
| Pod network | flannel (in-process) | Infra pods only |
| VM network | Multus (thin) + bridge CNI | One NAD per VLAN on `br0`, `ipam: {}`, LAN DHCP. VMs `autoattachPodInterface: false` |
| VM runtime | KubeVirt | `spec.infra.replicas: 1` |
| Images | CDI | DataVolumes, upload proxy, smart clone. Scratch on ZFS SC |
| Storage | OpenEBS ZFS LocalPV | Block mode, snapshots, clones, compression |
| UI | `kubevisor` (Go, single binary, in-cluster) | Serves web UI, local auth, talks to kube API as own ServiceAccount |

### UI

- Go binary, embedded static assets. Server-rendered HTML + htmx. noVNC + xterm.js for console.
- Auth: local users (bcrypt, stored in a Secret), app roles admin/operator/read-only. Binary uses its own scoped ServiceAccount; users never see Kubernetes. Later: OIDC, LDAP bind. SAML via OIDC broker.
- Console: proxy KubeVirt `vnc` / `console` subresource websockets.
- Scope: VMs (CRUD, power, console), disks (DataVolume), images (upload/import), networks (NAD), snapshots, node stats.

### Manager (later)

- Separate binary. Holds kubeconfig per node, aggregates inventory, reuses UI components.
- Cold migrate: `VirtualMachineExport` on source -> DataVolume import on target.
- Template/image library pushed to nodes.
- No shared storage. Live migration only if KubeVirt cross-cluster migration matures.

## Plan

| Stage | Deliverable | Done when |
|---|---|---|
| 0 | `os/`: Kairos image with k3s config + baked manifests (KubeVirt, CDI, Multus thin, ZFS LocalPV, NADs), ISO build, example cloud-config | Fresh host boots a VM from a CDI-imported cloud image on a LAN VLAN |
| 1 | `cmd/kubevisor`: auth, VM list/detail/power, console | Manage VM lifecycle + console from browser |
| 2 | UI: create VM, disks, images upload/import, networks, snapshots | No kubectl needed for day-to-day |
| 3 | Footprint pass: measure, trim requests/limits, reserved resources | Idle RAM documented on real hardware |
| 4 | `cmd/kubevisor-center`: node registry, aggregated view, cold migrate, templates | Move VM between two nodes from UI |
| 5 | HA restart (backup/replication based) | Open |

## Layout

```
os/              Kairos image (Dockerfile, rootfs overlay, cloud-config example)
cmd/kubevisor/   node UI binary
cmd/kubevisor-center/   manager (stage 4)
internal/        shared Go packages
web/             templates, static assets
docs/            architecture.drawio
```
