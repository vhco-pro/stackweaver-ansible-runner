# Stackweaver Ansible Runner

The self-hosted Ansible runner for the [Stackweaver](https://github.com/vhco-pro/stackweaver) DevOps platform.

> [!IMPORTANT]
> **This repository is auto-synced from the Stackweaver monorepo. Do not make changes here directly.**

## Overview

This runner executes Ansible playbooks as part of the Stackweaver orchestration pipeline. It connects to the Stackweaver API via Redis queue, receives jobs, and streams logs back in real-time. Includes built-in support for OIDC workload identity and common Ansible collections (AWS, Azure, GCP, VMware).

## Usage

```bash
docker pull ghcr.io/vhco-pro/stackweaver-ansible-runner:latest
```

See the [Stackweaver documentation](https://github.com/vhco-pro/stackweaver) for deployment and configuration instructions.

## Customize

Fork and adjust the dockerfile to create your own version of the runner image with any dependency you might require.

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
