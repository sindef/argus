# Argus

Lightweight KubeVirt hypervisor. One node = one standalone single-node k3s cluster (sqlite via kine), ESXi-style. Clustering is an optional manager on top, not a property of the nodes.

## Targets

| | Floor | Sane |
|---|---|---|
| CPU | 2 cores, VT-x/AMD-V | 4 cores |
| RAM (idle, no VMs) | 2.07GiB MemTotal−MemAvailable (fresh 1.71–2.16, post-activity 2.05–2.09; gate ≤2.2GiB; 4GiB node, ARC was ≤256MiB, now ≤128MiB) | 8GB+ |
| Disk | 10GiB OS + 10GiB ZFS datastore | NVMe |
| Per-VM overhead | ~150-250MB (virt-launcher) | |

## Architecture

See [docs/architecture.drawio](docs/architecture.drawio).

### Node

| Layer | Component | Notes |
|---|---|---|
| OS | Kairos (Ubuntu 26.04 base, k3s provider), KVM, `br0`, ZFS pool | Immutable A/B image built from `os/Dockerfile`. `br0` with VLAN filtering, uplink enslaved |
| K8s | k3s server, sqlite (kine), `GOMEMLIMIT=1GiB GOGC=50` (512MiB thrashed GC under VM load; containerd inherits both) | `--disable traefik,servicelb,metrics-server,local-storage --disable-cloud-controller --disable-network-policy --flannel-backend=host-gw --nonroot-devices` |
| Pod network | flannel (in-process) | Infra pods only |
| VM network | Multus (thin) + bridge CNI | One NAD per VLAN on `br0`, `ipam: {}`, LAN DHCP. VMs `autoattachPodInterface: false` |
| VM runtime | KubeVirt | `spec.infra.replicas: 1` |
| Images | CDI | DataVolumes, upload proxy, smart clone. Scratch on ZFS SC |
| Storage | ZFS pool `argus` + OpenEBS ZFS LocalPV | `argus/rancher` holds `/var/lib/rancher` (k3s state, images). VM disks are zvols: block mode, snapshots, clones, compression |
| UI | `argus` (Go, single binary, in-cluster) | Serves web UI, local auth, talks to kube API as own ServiceAccount |

### UI

- Go binary, embedded static assets. Server-rendered HTML + htmx. noVNC + xterm.js for console.
- Auth: local users (bcrypt, Secret `argus-system/argus-users`), roles admin/operator/read-only. Initial `admin` password: from `/etc/argus/admin-password` if cloud-config writes it, otherwise random and shown on the node console. Change forced on first login. Admins manage users at `/users`; new and reset users get a one-time password. Binary uses its own scoped ServiceAccount; users never see Kubernetes. Later: OIDC, LDAP bind. SAML via OIDC broker.
- Host: OS updates, reboot and shutdown are requested through `/run/argus/request`; a host unit (`argus-host`) runs `kairos-agent upgrade` and reboots. Refused while VMs run. Insecure registries via `upgrade.allow-insecure-registries` in cloud-config.
- Serves HTTPS on node port 443. Self-signed cert generated on first start, kept in Secret `argus-system/argus-tls`.
- Image baked into the OS image and imported by k3s at boot; no registry needed.
- Console: proxy KubeVirt `vnc` / `console` subresource websockets.
- Scope: VMs (CRUD, power, console), disks (DataVolume), images (upload/import), networks (NAD), snapshots, node stats.

### Argus Manager (`argus-manager`)

- Separate binary/container, own local users, state in a data dir (`ARGUS_DATA`). Never touches Kubernetes.
- Talks to each node's argus over HTTPS (`/api/...`) with a per-node API token, trusting only that node's CA (pinned by fingerprint at registration, like a host thumbprint).
- Node certs are issued by a per-node CA and include the node IP (`HOST_IP`), so nodes can verify each other during migration.
- Aggregated VM view with power actions; links into each node's UI for detail and console.
- Cold migrate (VM off, no CD-ROM): source exports via KubeVirt `VirtualMachineExport`, argus streams it at `/export/...` guarded by the export token; target imports with CDI straight from the source. Disks never pass through the manager. Source VM is deleted after the target has all disks. VMs keep their MAC.
- Target needs the same network names (VLANs) as the source.
- Live migrate (VM running): KubeVirt decentralized live migration (feature gate `DecentralizedLiveMigration`, alpha). The manager's migration network setting (VLAN + subnet ≥ /24) gives each node a fixed 16-address block and creates `kubevirt/argus-migration` on `br0`; virt-handler and the sync controller join it as `migration0`. Nodes need L2 on that VLAN. KubeVirt CAs are re-exchanged before each migration. Measured ~1s ping loss for a small VM.
- argus restarts KubeVirt's sync controller before a live migration if its lease is stale: on lease loss it stops working without exiting.

HA (per VM, opt-in from the manager):
- Protect VM → pick replica node + interval. Manager replicates on the interval: online snapshot on the owner, export, full copy into a stopped replica on the replica node. Crash-consistent, full copy each time (no incremental yet). Replica is unavailable while it is being refreshed.
- Protected VMs run with `runStrategy: Manual`; nothing starts on its own after a reboot. Manager heartbeats every node every 10s with the VMs it owns; node argus runs owned VMs, keeps others stopped, respects user power-off.
- Failover: owner unreachable for 2 min and the replica node can't reach it either → ownership moves to the replica node, which starts the replica. The old node's copy stays off when it returns; replication then runs the other way.
- Fencing: host timer `argus-fence` kills protected VMs' qemu if the node has had no heartbeat for 60s and can reach no peer. Independent of Kubernetes, which is not reliably reachable from pods while isolated.
- Manager down: VMs keep running (peers reachable, no fencing); no failover until it returns.
- VM names must be unique across nodes.

```
make manager
docker run -d -p 443:8443 -e ARGUS_DATA=/home/nonroot/data -v argus-manager:/home/nonroot argus-manager:0.1.0
docker logs <container>    # initial admin password
```

Add a node: on the node's Host page create a token and copy the CA fingerprint; in the manager, Nodes → Add node.

## Plan

| Stage | Deliverable | Done when |
|---|---|---|
| 0 | `os/`: Kairos image with k3s config + baked manifests (KubeVirt, CDI, Multus thin, ZFS LocalPV, NADs), ISO build, example cloud-config | Fresh host boots a VM from a CDI-imported cloud image on a LAN VLAN |
| 1 | `cmd/argus`: auth, VM list/detail/power, console | Manage VM lifecycle + console from browser |
| 2 | UI: create VM, disks, images upload/import, networks, snapshots | No kubectl needed for day-to-day |
| 3 | Footprint pass: measure, trim requests/limits, reserved resources | Idle RAM documented on real hardware |
| 4 | Host page: OS update from a registry image, reboot, shutdown | Node updates itself from the UI, data intact |
| 5 | `cmd/argus-manager`: node registry, aggregated view, cold migrate, image library | Move VM between two nodes from UI |
| 6 | Live migration between nodes (KubeVirt decentralized migration, migration VLAN) | Running VM moves between nodes |
| 7 | HA restart: replication, heartbeats, failover, host fencing | VM restarts on another node after host failure |

## Layout

```
os/                Kairos image (Dockerfile, rootfs overlay, cloud-config example)
cmd/argus/         node UI binary
cmd/argus-manager/  manager (stage 4)
internal/          shared Go packages
web/               templates, static assets
docs/              architecture.drawio
```
