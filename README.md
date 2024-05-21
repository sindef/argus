# Argus

Lightweight KubeVirt hypervisor on Kairos and K3s. Each host runs an independent, single-node cluster with local storage. Optional **Argus Manager** coordinates hosts, migrations and replicas without accessing Kubernetes directly.

**Experimental alpha.** Recovery, network partitions and hardware upgrades need further validation. Use disposable workloads and a trusted management network. Automatic failover is disabled by default; enabling it without verified fencing can start two copies of a VM.

## Footprint

| Resource | Minimum / target |
|---|---|
| CPU | 2 cores with VT-x or AMD-V; 4 cores recommended |
| Host memory | 4 GiB for small test nodes; 8 GiB or more recommended |
| Idle memory | Previously recorded around 1.9 GiB on a 4 GiB nested node; acceptance ceiling 2.2 GiB |
| Disk | Separate OS and VM disks; at least 10 GiB each for testing |
| VM overhead | Previously estimated at 150–250 MiB per running VM, plus guest memory |


## Architecture

See [architecture diagram](docs/architecture.drawio).

- **Host:** Kairos, Ubuntu 26.04 base, KVM, systemd networking and ZFS. Immutable OS images support A/B upgrades.
- **Kubernetes:** single-node K3s using SQLite through Kine. Traefik, ServiceLB, metrics-server, local-storage, cloud controller and network policy controller are disabled. Kubernetes Secrets encryption is enabled.
- **VM runtime:** KubeVirt, CDI for disk import/upload, Multus and bridge CNI for guest networking. Infrastructure controllers run with reduced replicas and tuned Go memory settings.
- **Storage:** OpenEBS ZFS LocalPV. VM disks are local zvols with snapshots and clones. Hosts do not aggregate storage or form an HCI storage cluster; migrations copy disks between hosts. SAN and additional CSI providers remain future work.
- **Node UI:** one Go binary with embedded HTML, htmx, noVNC and xterm.js. A socket-activated host helper handles privileged disk, network and backup operations.
- **Manager:** separate Go binary/container with its own users, organizations and persistent configuration. Host traffic uses HTTPS, node API tokens and pinned node CAs.

## Features and limits

### Host

VM lifecycle, hardware, disks, image import/upload, networks, snapshots and consoles are managed through the UI. Local accounts use bcrypt and admin/operator/read-only roles. First login requires a password change; initial credentials appear on the host console or come from a configured password file.

HTTPS is exposed on port 443 with a certificate issued by the host's own CA. A custom web certificate can be installed. Keep management access separated from guest and untrusted networks; the example configuration enables the management firewall on `br0`.

OS update, reboot and shutdown operations require running VMs to be stopped. Scheduled backups support local snapshots and full offsite copies to an NFS target. Offsite VM backups reject separately attached volumes rather than silently omitting their disks; completed backups include checksums verified before restore.

### Manager

Manager aggregates host/VM inventory, consoles, storage and networking. It supports local users, organizations, scoped access and OIDC sign-in. Configuration backups use a passphrase; restore is an offline command.

- **Cold migration:** VM stopped, supported disks and compatible destination networks. Target imports directly from source; Manager does not relay disk data. Source deletion follows completed target disk imports.
- **Live migration:** experimental KubeVirt decentralized migration. Hosts need compatible networks and L2 connectivity on the configured migration network. Disk copying makes this slower than reattaching shared storage.
- **Replication:** per-VM, opt-in, crash-consistent snapshots and full disk copies. Refresh stages replacement disks while retaining the previous replica until swap. Failure recovery around an uncertain swap acknowledgement remains under review.
- **Failover:** TBA. Working today, but manual.

Use unique protected VM names across hosts. Treat registered hosts as trusted: Manager still incorporates their UI responses.

## Build and try

Source checks use Go **1.27.2**, matching CI and container builds. Docker with Buildx is required for images and ISO generation.

```sh
go test ./...
go vet ./...
go build ./...
make manager
make iso
```

`make iso` writes the ISO under `build/`. Adapt [example cloud-config](os/cloud-config.example.yaml), replacing the SSH key, uplink and disk identifiers. Its automatic installer writes to the selected OS disk; use disposable disks for initial testing.

Run Manager with persistent storage:

```sh
docker run -d --name argus-manager -p 443:8443 \
  -e ARGUS_DATA=/home/nonroot/data \
  -v argus-manager:/home/nonroot argus-manager:0.1.0
docker logs argus-manager
```

Open Manager over HTTPS, change the initial admin password, then register a host using its URL, API token and CA fingerprint. Keep the configuration volume when replacing the container.

## Development and releases

[CONTRIBUTING.md](CONTRIBUTING.md) covers local checks, contribution scope and AI-assisted work. [AGENTS.md](AGENTS.md) gives instructions for coding agents. Report vulnerabilities through [SECURITY.md](SECURITY.md).

`master` is the default and production branch. Branch pushes and pull requests run CI. Version tags run the same checks at the tagged commit before publishing signed images with SBOMs and provenance; tagged commits must belong to `master`. TruffleHog scans full fetched Git history, including unverified findings, without contacting credential providers.

## Layout

```text
cmd/argus/          Host UI and API
cmd/argus-manager/  Fleet coordinator
cmd/argus-hostd/    Privileged host helper
internal/          Shared Go packages
web/               Embedded templates and static assets
os/                Kairos image, manifests and cloud-config
docs/              Architecture diagram
```

## License

Copyright 2026 Argus contributors. Argus-authored code is licensed under **GNU Affero General Public License v3.0 only** (`AGPL-3.0-only`); see [LICENSE](LICENSE). Third-party dependencies, vendored assets and OS components retain their own licenses and notices.
