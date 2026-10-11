# Argus

Lightweight KubeVirt hypervisor on Kairos and K3s, with an optional fleet coordinator: **Argus Manager**.

**Experimental alpha.** See [security and deployment limits](SECURITY.md).

## Architecture

Each host runs an independent single-node K3s cluster with SQLite, KubeVirt, CDI, Multus/bridge CNI and OpenEBS ZFS LocalPV. The OS is Kairos on Ubuntu 26.04 with KVM.

Storage is local per host. Migration and replication copy VM data between hosts. Manager uses host HTTPS APIs with node tokens and pinned CAs.

[Architecture diagram](docs/architecture.drawio)

## Host

- VM lifecycle, disks, image import/upload and VNC/serial consoles.
- ZFS pools, snapshots and clones.
- Bridges, VLANs and management networking.
- Local users and role-based access.
- Scheduled snapshots and NFS backups.
- OS upgrades, reboot and shutdown.

## Manager

- Fleet inventory, host groups and consoles.
- VM templates and placement.
- Cold migration and experimental live migration.
- Scheduled replication and manual failover.
- Organizations, scoped access, local users and OIDC.
- Encrypted configuration backups.

Automatic failover is disabled by default.

## Requirements

| Resource | Minimum / target |
|---|---|
| CPU | 2 cores with VT-x or AMD-V; 4 recommended |
| Host memory | 4 GiB for testing; 8 GiB or more recommended |
| Idle memory | Previously measured around 1.9 GiB; target ceiling 2.2 GiB |
| Disk | Separate OS and VM disks; at least 10 GiB each for testing |
| VM overhead | Estimated 150–250 MiB per VM, plus guest memory |

## Build and run

Go **1.27.2** and Docker with Buildx.

```sh
make manager
make iso
```

ISO output: `build/`. Set the SSH key, uplink and disk identifiers in [cloud-config](os/cloud-config.example.yaml). The installer overwrites the selected OS disk.

Run Manager with persistent storage:

```sh
docker run -d --name argus-manager -p 443:8443 \
  -e ARGUS_DATA=/home/nonroot/data \
  -v argus-manager:/home/nonroot argus-manager:0.1.0
docker logs argus-manager
```

Open HTTPS, change the initial admin password, then register hosts using their URL, API token and CA fingerprint.

## Development

[Contributing and AI policy](CONTRIBUTING.md) · [Agent guidelines](AGENTS.md) · [Security reporting](SECURITY.md)

`master` is the default and production branch. CI includes secret scanning. Version tags publish signed images with SBOMs and provenance after CI passes; tagged commits must belong to `master`.

## License

[GNU AGPLv3 only](LICENSE) (`AGPL-3.0-only`). Third-party components retain their own licenses and notices.
