# Stackweaver Ansible Runner

The self-hosted Ansible runner for the [Stackweaver](https://sw.vhco.pro) DevOps platform.

This is the public release repository for the Stackweaver Ansible runner. It is published from the Stackweaver source tree on every release. See the [release sync architecture](https://sw.vhco.pro/docs/security/sync-architecture) for how releases are built, signed, and mirrored here.

## Overview

This runner executes Ansible playbooks as part of the Stackweaver orchestration pipeline. It connects to the Stackweaver API via Redis queue, receives jobs, and streams logs back in real-time. Includes built-in support for OIDC workload identity and common Ansible collections (AWS, Azure, GCP, VMware).

## Usage

```bash
docker pull ghcr.io/vhco-pro/stackweaver-ansible-runner:latest
```

See the [Stackweaver documentation](https://sw.vhco.pro/docs) for deployment and configuration instructions.

## Customize

Fork and adjust the dockerfile to create your own version of the runner image with any dependency you might require.

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
