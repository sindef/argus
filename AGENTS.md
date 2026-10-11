# Coding agents

Read [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md) and the relevant source before making changes. These repository instructions supplement higher-priority workspace and user instructions.

## Ownership

- Follow the AI contribution policy in `CONTRIBUTING.md`.
- Use the [PR template](.github/pull_request_template.md). Leave `HUMAN ONLY` sections and human disclosure checkboxes untouched.
- AI may write factual summaries and report checks it actually ran. Do not invent rationale, test evidence, measurements, issue references or contributor history.
- Leave public issue/proposal text, review replies and discussion to the contributor. Do not configure automatic agent replies.

## Implementation

- Keep one task per change. Preserve unrelated edits and use focused commits.
- Keep each Argus host independent. Prefer Manager for orchestration; do not introduce distributed host storage without an agreed design.
- Use shipped Kairos, K3s, KubeVirt and CDI versions as the configuration authority. Verify unfamiliar APIs against source or official documentation.
- Preserve organization, host-scope and role checks. Treat remote HTML, backup contents and storage inventories as untrusted input.
- Treat lost responses to mutations as uncertain outcomes. Reconcile durable state before retrying destructive cleanup or starting another VM copy.
- Do not modify a running Kubernetes cluster directly. Cluster changes must go through GitOps; read-only inspection is allowed.
- Add dependencies, services and abstractions only when the task requires them. Measure host footprint changes.
- Edit documentation and add code comments only when requested. Avoid unrelated notes, generated files and broad formatting changes.
- Add regression tests for meaningful behaviour and failure paths, not trivial assertions.

## Validation

Run the relevant checks from `CONTRIBUTING.md`; use Go 1.27.2 for CI parity. Validate workflow changes with actionlint when available. Verify your own diff for scope, security, licensing and unnecessary complexity. Distinguish source/mock checks from hardware or cluster validation. Report remaining uncertainty plainly.
