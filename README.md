# Stackweaver™ Ansible Runner

[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/vhco-pro/stackweaver-ansible-runner/badge)](https://scorecard.dev/viewer/?uri=github.com/vhco-pro/stackweaver-ansible-runner)

The self-hosted Ansible runner for the [Stackweaver](https://sw.vhco.pro) DevOps platform.

This is the public release repository for the Stackweaver Ansible Runner. It is published from the Stackweaver source tree on every release. See the [release sync architecture](https://sw.vhco.pro/docs/security/sync-architecture) for how releases are built, signed, and mirrored here.

## Overview

This runner executes Ansible playbooks as part of the Stackweaver orchestration pipeline. It connects to the Stackweaver API via Redis queue, receives jobs, and streams logs back in real-time. Includes built-in support for OIDC workload identity and common Ansible collections (AWS, Azure, GCP, VMware).

## Usage

```bash
docker pull ghcr.io/vhco-pro/stackweaver-ansible-runner:latest
```

See the [Stackweaver documentation](https://sw.vhco.pro/docs) for deployment and configuration instructions.

## Customize

Fork and adjust the dockerfile to create your own version of the runner image with any dependency you might require.


## Verifying this Distribution

Every image published by this satellite is Sigstore-signed (keyless, via Fulcio + Rekor) and ships with build-provenance and SBOM attestations. To verify a specific tag:

```bash
cosign verify \
  --certificate-identity-regexp "^https://github\.com/vhco-pro/stackweaver-ansible-runner/\.github/workflows/release\.yml@refs/tags/.+$" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/vhco-pro/stackweaver-ansible-runner:<tag>
```

The full verification guide including SBOM extraction, SLSA provenance (live after the Wave-6 visibility flip), and `gitsign verify` for sync commits lives at <https://sw.vhco.pro/docs/security/verifying-releases>.

## Trademark

Stackweaver™ is a trademark of VH & Co. The Stackweaver name and word mark identify the official Stackweaver project; the source-code licence does not grant a right to use the mark in product names, hosted services, or company names. See the [Trademark Policy](https://github.com/vhco-pro/.github/blob/main/TRADEMARK.md) for the full terms.

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
