# Contributing

Discuss substantial architecture, storage or security changes with maintainers before implementation. Include a reproducible example for bugs; report vulnerabilities through [SECURITY.md](SECURITY.md).

## Scope

Keep hosts small and independently usable. Put fleet coordination in Manager. Storage remains local per host; propose shared or aggregated storage separately. Avoid new controllers, dependencies and background services when existing components suffice. Record memory impact when adding host processes or increasing controller resources.

Use one focused change per pull request and small commits per feature or fix. Describe the trigger, resulting behaviour and validation. Call out compatibility, migration or recovery implications. Use a Conventional Commit title such as `fix(backup): Preserve completed replica after an uncertain swap`.

## Checks

Use Go 1.27.2 and run from the repository root:

```sh
test -z "$(gofmt -l cmd internal web)"
go vet ./...
go test ./...
go build ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
for f in os/rootfs/usr/sbin/*; do sh -n "$f"; done
```

Docker/Buildx checks are `make manager`, `make app` and `make hostd`. Run `make iso` when changing the OS image or boot configuration. Test authentication boundaries, destructive storage actions and uncertain outcomes with meaningful regression cases; avoid tests that only repeat implementation details. State which checks ran and which need hardware or a nested lab.

CI uses checksum-pinned TruffleHog 3.99.2. With that version installed, scan history before pushing:

```sh
trufflehog git "file://$PWD" --no-verification --no-update --fail --fail-on-scan-errors
```

Findings block CI even without live credential verification. Review false positives narrowly; never suppress a real credential. Rotate exposed credentials before removing them from history.

## AI-assisted contributions

AI can help implement code, meaningful tests, repository documentation and factual PR summaries. The submitter must inspect the result, understand its behaviour, verify its provenance and run appropriate checks.

Humans write submitted issues and proposals, review conclusions, review replies and PR sections marked `HUMAN ONLY`. Agents must leave those fields and disclosure checkboxes untouched. Automated conversation on a contributor's behalf requires explicit maintainer approval.

Disclose substantial AI implementation in the PR template; small autocomplete edits need no disclosure. Never invent test results, measurements, issue references or design history. Validate generated Kubernetes/KubeVirt configuration against the versions Argus actually ships. Keep credentials and private infrastructure data out of external AI services.

These expectations take general inspiration from [Grafana Alloy's AI policy](https://github.com/grafana/alloy/blob/main/docs/developer/genai.md). Argus's own policy governs contributions; Alloy's tools, CLA and repository procedures do not apply here.

## Licensing

Submit only work you have the right to contribute under [AGPL-3.0-only](LICENSE). Preserve third-party notices and check compatibility before adding dependencies or generated code of uncertain origin.
