# Ansible Runner Dockerfile
# This builds the Ansible runner that executes Ansible playbooks
# Build context must be the repository root (context: .)

# Build stage
FROM golang:1.25.7-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go modules first for caching
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# Copy source code
COPY backend/ .

# Build the ansible-runner binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ansible-runner ./cmd/ansible-runner

# Runtime stage
FROM python:3.14-slim

# Install Ansible and dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    openssh-client \
    git \
    sshpass \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install Ansible (latest stable)
RUN pip install --no-cache-dir \
    ansible \
    ansible-lint \
    jmespath \
    netaddr \
    boto3 \
    azure-identity \
    azure-mgmt-resource \
    azure-mgmt-compute \
    azure-mgmt-network \
    azure-mgmt-subscription \
    azure-cli-core \
    google-auth \
    pyvmomi

# Install additional Ansible collections commonly used
RUN ansible-galaxy collection install \
    amazon.aws \
    azure.azcollection \
    google.cloud \
    community.vmware \
    community.general \
    ansible.posix \
    ansible.netcommon

# Copy the runner binary from builder
COPY --from=builder /app/ansible-runner /usr/local/bin/ansible-runner

# Copy the OIDC-aware ansible-inventory wrapper (must be before USER iac)
# This monkey-patches the Azure RM collection to use azure-identity's
# native WorkloadIdentityCredential when OIDC env vars are present.
COPY scripts/oidc-ansible-inventory /usr/local/bin/oidc-ansible-inventory
RUN chmod +x /usr/local/bin/oidc-ansible-inventory

# Create non-root user with UID 1001 to match terraform runner (shared volume)
RUN useradd -m -u 1001 iac
USER iac

# Create workspaces directory (ansible-specific subdirs)
# Also create galaxy cache directories for persistent collection storage
RUN mkdir -p /home/iac/workspaces/ansible-sync /home/iac/workspaces/ansible-jobs \
    /home/iac/galaxy-cache/collections /home/iac/galaxy-cache/roles

# Set working directory
WORKDIR /home/iac

# Environment variables
ENV WORKSPACES_DIR=/home/iac/workspaces
ENV ANSIBLE_HOST_KEY_CHECKING=false
ENV ANSIBLE_RETRY_FILES_ENABLED=false

# Run the ansible-runner
CMD ["/usr/local/bin/ansible-runner"]
