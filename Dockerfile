# Ansible job image Dockerfile
# Sync smoke test 2026-05-25T17:08Z (D-SYNC-PR)
# This builds the Ansible job image: the image Ansible jobs and syncs execute in, which also
# starts an in-process launcher for Ansible by default (one-launcher spec, A5, OD4).
# NOTE: This Dockerfile must be built with context set to the repository root
# Example: docker build -f runner-images/ansible/Dockerfile -t ansible-runner .

# ── Build stage: compile the Go binary ──────────────────────────────
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder
ENV GOPRIVATE=github.com/michielvha/stackweaver

WORKDIR /app
RUN apk add --no-cache git

COPY backend/go.mod backend/go.sum ./
RUN --mount=type=secret,id=netrc,target=/root/.netrc go mod download

COPY backend/ .
# The launcher binary carries every engine's executor; VERSION is the release it reports.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags "-X main.version=${VERSION}" -o stackweaver-launcher ./cmd/launcher

# ── Python deps stage: install via uv into a venv ───────────────────
FROM python:3.14-slim@sha256:a2b82f3c48559aa0a8446d9af49826b6e2b2016f4cd2afabfe6013ec53729170 AS python-deps

COPY --from=ghcr.io/astral-sh/uv:0.13.1@sha256:4a1f3666816dafadd20ceb71eac9a7d56d7ef30279f7d64048519ab901ada336 /uv /usr/local/bin/

WORKDIR /opt/ansible-deps
COPY runner-images/ansible/pyproject.toml runner-images/ansible/uv.lock ./
RUN uv sync --frozen --group all --no-dev --no-install-project

# Install azure.azcollection's full Python SDK dependency set. The azure_rm
# inventory plugin and modules import dozens of azure-mgmt-* / msgraph clients at
# load time; without them the native AZURE_FEDERATED_TOKEN_FILE (workload identity)
# auth path fails to import (ClientAssertionCredential never defined), so dynamic
# inventory sync falls back and fails with "Failed to get credentials". Sourced
# from the collection's own requirements.txt (bundled with the ansible package) so
# the pins always match the installed collection version and supersede the
# narrower hand-picked azure-mgmt pins above.
#
# The collection pins azure-cli-core to a release whose msal caps cryptography at an old
# version, so an unconstrained install here downgrades the CLI, auth and crypto packages below
# what uv.lock resolved - and below their security fixes - while the alert on uv.lock closes.
# The lock therefore stays authoritative for those packages (--override), and the collection's
# pins govern only the azure-mgmt-* / msgraph clients it needs. `uv pip check` fails the build
# if the two ever stop being compatible.
RUN uv export --frozen --group all --no-dev --no-emit-project --no-hashes \
      | grep -E '^(azure-cli-core|azure-cli-telemetry|azure-core|msal|msal-extensions|cryptography|pyopenssl|knack|pyjwt|urllib3|oauthlib)==' \
      > /tmp/lock-overrides.txt && \
    test -s /tmp/lock-overrides.txt && \
    req="$(find .venv -path '*azure/azcollection/requirements.txt' | head -1)" && \
    test -n "$req" && \
    uv pip install --python /opt/ansible-deps/.venv/bin/python -r "$req" --override /tmp/lock-overrides.txt && \
    uv pip check --python /opt/ansible-deps/.venv/bin/python

# ── Runtime stage ────────────────────────────────────────────────────
FROM python:3.14-slim@sha256:a2b82f3c48559aa0a8446d9af49826b6e2b2016f4cd2afabfe6013ec53729170

# System packages required by Ansible modules / connections (upgrade first to pull security patches);
# iptables and nftables are for the launcher's one-shot jobs-network firewall writer (#933, D5)
# Remove pip/ensurepip from stdlib - uv handles package management, pip is a vulnerability surface
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        openssh-client git sshpass ca-certificates iptables nftables \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /usr/local/lib/python3.14/ensurepip /usr/local/lib/python3.14/site-packages/pip* \
    && ln -sf /usr/local/bin/python3 /usr/bin/python3

# Copy the pre-built venv from the python-deps stage
COPY --from=python-deps /opt/ansible-deps/.venv /opt/ansible-deps/.venv

# Make the venv active so every ansible command is found. The collections a job loads are
# the ones the ansible package in this venv bundles (site-packages/ansible_collections), so
# the ansible version in pyproject.toml and uv.lock decides the collection set.
ENV VIRTUAL_ENV=/opt/ansible-deps/.venv
ENV PATH="/opt/ansible-deps/.venv/bin:$PATH"

# The launcher binary at the constant executor path, and a link at the old path, so an older
# launcher that runs `/usr/local/bin/ansible-runner executor` in this image still starts its
# executor.
COPY --from=builder /app/stackweaver-launcher /usr/local/bin/stackweaver-launcher
RUN ln -s stackweaver-launcher /usr/local/bin/ansible-runner

# Note: the former oidc-ansible-inventory wrapper is retired - azure.azcollection >= 3.17.0 reads
# AZURE_FEDERATED_TOKEN_FILE natively, so inventory sync runs plain ansible-inventory for every
# auth mode. The script is kept as a historical artifact under backend/scripts/archive/.

# Create non-root user with UID 1001 to match the OpenTofu runner (shared volume). The UID, not
# the name, is what matters there - it stayed 1001 across the iac -> stackweaver rename.
RUN useradd -m -u 1001 stackweaver
# A launcher's node-local, read-only cache copy (RUNNER_CACHE_DIR): owned by the image's user, so a
# named volume mounted here starts out writable by an in-process launcher too.
RUN mkdir -p /var/cache/stackweaver-launcher && chown stackweaver:stackweaver /var/cache/stackweaver-launcher
USER stackweaver

# Working directories
RUN mkdir -p /home/stackweaver/workspaces/ansible-sync \
             /home/stackweaver/workspaces/ansible-jobs

WORKDIR /home/stackweaver

# Environment
ENV WORKSPACES_DIR=/home/stackweaver/workspaces
# Default Ansible home under writable /tmp so Ansible can create its local temp
# dir under the read-only root filesystem. The runner overrides this per
# job/sync onto the workspaces volume; the Galaxy cache lives on that volume too.
ENV ANSIBLE_HOME=/tmp/.ansible
# Module "remote" temp dir. For local-connection tasks the target is this
# read-only container, so it must live under writable /tmp (per-task namespaced).
ENV ANSIBLE_REMOTE_TMP=/tmp/.ansible/tmp

# Start an in-process launcher for Ansible
CMD ["/usr/local/bin/stackweaver-launcher"]
