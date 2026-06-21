# Ansible Runner Dockerfile
# Sync smoke test 2026-05-25T17:08Z (D-SYNC-PR)
# This builds the Ansible runner that executes Ansible playbooks
# NOTE: This Dockerfile must be built with context set to the repository root
# Example: docker build -f runner-images/ansible/Dockerfile -t ansible-runner .

# ── Build stage: compile the Go binary ──────────────────────────────
FROM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder
ENV GOPRIVATE=github.com/michielvha/stackweaver

WORKDIR /app
RUN apk add --no-cache git

COPY backend/go.mod backend/go.sum ./
RUN --mount=type=secret,id=netrc,target=/root/.netrc go mod download

COPY backend/ .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ansible-runner ./cmd/ansible-runner

# ── Python deps stage: install via uv into a venv ───────────────────
FROM python:3.14-slim@sha256:44dd04494ee8f3b538294360e7c4b3acb87c8268e4d0a4828a6500b1eff50061 AS python-deps

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/

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
RUN req="$(find .venv -path '*azure/azcollection/requirements.txt' | head -1)" && \
    test -n "$req" && \
    uv pip install --python /opt/ansible-deps/.venv/bin/python -r "$req"

# ── Runtime stage ────────────────────────────────────────────────────
FROM python:3.14-slim@sha256:44dd04494ee8f3b538294360e7c4b3acb87c8268e4d0a4828a6500b1eff50061

# System packages required by Ansible modules / connections (upgrade first to pull security patches)
# Remove pip/ensurepip from stdlib — uv handles package management, pip is a vulnerability surface
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        openssh-client git sshpass ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /usr/local/lib/python3.14/ensurepip /usr/local/lib/python3.14/site-packages/pip* \
    && ln -sf /usr/local/bin/python3 /usr/bin/python3

# Copy the pre-built venv from the python-deps stage
COPY --from=python-deps /opt/ansible-deps/.venv /opt/ansible-deps/.venv

# Make the venv active so ansible-galaxy and all ansible commands are found
ENV VIRTUAL_ENV=/opt/ansible-deps/.venv
ENV PATH="/opt/ansible-deps/.venv/bin:$PATH"

# Install Ansible Galaxy collections from pinned requirements
WORKDIR /opt/ansible-deps
COPY runner-images/ansible/requirements.yml ./
RUN ansible-galaxy collection install -r requirements.yml

# Copy the Go binary from the builder stage
COPY --from=builder /app/ansible-runner /usr/local/bin/ansible-runner

# Note: the former oidc-ansible-inventory wrapper is retired — azure.azcollection >= 3.17.0 reads
# AZURE_FEDERATED_TOKEN_FILE natively, so inventory sync runs plain ansible-inventory for every
# auth mode. The script is kept as a historical artifact under backend/scripts/archive/.

# Create non-root user with UID 1001 to match terraform runner (shared volume)
RUN useradd -m -u 1001 iac
USER iac

# Working directories
RUN mkdir -p /home/iac/workspaces/ansible-sync \
             /home/iac/workspaces/ansible-jobs

WORKDIR /home/iac

# Environment
ENV WORKSPACES_DIR=/home/iac/workspaces
# Default Ansible home under writable /tmp so Ansible can create its local temp
# dir under the read-only root filesystem. The runner overrides this per
# job/sync onto the workspaces volume; the Galaxy cache lives on that volume too.
ENV ANSIBLE_HOME=/tmp/.ansible
# Module "remote" temp dir. For local-connection tasks the target is this
# read-only container, so it must live under writable /tmp (per-task namespaced).
ENV ANSIBLE_REMOTE_TMP=/tmp/.ansible/tmp

CMD ["/usr/local/bin/ansible-runner"]
