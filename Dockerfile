# Ansible Runner Dockerfile
# This builds the Ansible runner that executes Ansible playbooks
# NOTE: This Dockerfile must be built with context set to the repository root
# Example: docker build -f runner-images/ansible/Dockerfile -t ansible-runner .

# ── Build stage: compile the Go binary ──────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ansible-runner ./cmd/ansible-runner

# ── Python deps stage: install via uv into a venv ───────────────────
FROM python:3.14-slim AS python-deps

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/

WORKDIR /opt/ansible-deps
COPY runner-images/ansible/pyproject.toml runner-images/ansible/uv.lock ./
RUN uv sync --frozen --group all --no-dev --no-install-project

# ── Runtime stage ────────────────────────────────────────────────────
FROM python:3.14-slim

# System packages required by Ansible modules / connections (upgrade first to pull security patches)
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        openssh-client git sshpass ca-certificates \
    && rm -rf /var/lib/apt/lists/*

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

# Copy the OIDC-aware ansible-inventory wrapper
COPY backend/scripts/oidc-ansible-inventory /usr/local/bin/oidc-ansible-inventory
RUN chmod +x /usr/local/bin/oidc-ansible-inventory

# Create non-root user with UID 1001 to match terraform runner (shared volume)
RUN useradd -m -u 1001 iac
USER iac

# Working directories
RUN mkdir -p /home/iac/workspaces/ansible-sync \
             /home/iac/workspaces/ansible-jobs \
             /home/iac/galaxy-cache/collections \
             /home/iac/galaxy-cache/roles

WORKDIR /home/iac

# Environment
ENV WORKSPACES_DIR=/home/iac/workspaces
ENV ANSIBLE_HOST_KEY_CHECKING=false
ENV ANSIBLE_RETRY_FILES_ENABLED=false

CMD ["/usr/local/bin/ansible-runner"]
