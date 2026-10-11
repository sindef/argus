# Security

## Supported versions

Argus is experimental. Fixes target the latest `master`; there are no supported production releases or maintenance branches yet. Update to the latest fixed commit when a vulnerability is resolved.

## Report privately

Use **Security → Report a vulnerability** at [sindef/argus](https://github.com/sindef/argus/security/advisories/new). Do not put exploit details, credentials or sensitive logs in public issues or pull requests.

Include affected commit/version, prerequisites, reproduction steps, impact and a minimal sanitized example. For data-loss reports, describe VM state, storage layout and the failure sequence. Test only systems you own or have permission to assess.

Maintainers will triage reports privately and coordinate fixes and disclosure with the reporter. There is no guaranteed response time or security bounty.

## Deployment boundaries

- Restrict host and Manager access to trusted management networks. Keep guest access separate.
- Registered hosts are trusted peers. Manager uses privileged node tokens; host-page isolation still needs hardening.
- Automatic failover lacks verified fencing. Leave it disabled and fence the owner before manual promotion.
- Validate backup restore, interrupted migration, replica refresh and upgrade recovery on disposable workloads before relying on them.
- Do not commit real credentials, Manager state, backup archives or private keys. CI secret scanning does not replace credential rotation after exposure.

## Maintainer setup

Private reporting is a GitHub repository setting; adding this file does not enable it. After creating the repository, enable and verify it:

```sh
gh api --method PUT repos/sindef/argus/private-vulnerability-reporting
gh api repos/sindef/argus/private-vulnerability-reporting --jq .enabled
```
