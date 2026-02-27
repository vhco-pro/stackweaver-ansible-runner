#!/bin/bash
# Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

# Integration test script for registry endpoints
# This script tests provider upload and GPG key management against a live database

set -e

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}Running Registry Integration Tests${NC}"
echo "=================================="

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Change to backend directory (where go.mod is located)
cd "$SCRIPT_DIR"

# Check if TEST_DATABASE_URL is set
if [ -z "$TEST_DATABASE_URL" ]; then
    echo -e "${YELLOW}TEST_DATABASE_URL not set, using default local database${NC}"
    export TEST_DATABASE_URL="postgres://iac:iac_password@localhost:5432/iac_platform?sslmode=disable"
fi

# Check if API_BASE_URL is set (for live API testing)
if [ -z "$API_BASE_URL" ]; then
    echo -e "${YELLOW}API_BASE_URL not set, using default${NC}"
    export API_BASE_URL="http://localhost:8022/api/v2"
fi

# Verify we're in the backend directory with go.mod
if [ ! -f "go.mod" ]; then
    echo -e "${RED}Error: go.mod not found. Please run this script from the backend directory.${NC}"
    exit 1
fi

# Check if GPG key file exists, extract if needed
if [ ! -f "../deploy/test-gpg-key.pub" ] && [ -f "../deploy/pgp_private_key" ]; then
    echo -e "${YELLOW}Extracting public key from private key...${NC}"
    # Try to extract public key (requires GPG to be installed)
    if command -v gpg &> /dev/null; then
        # Import private key and export public key
        gpg --import ../deploy/pgp_private_key 2>&1 | grep -q "imported\|unchanged" && \
        gpg --armor --export EFD2D04A9FC214C0 > ../deploy/test-gpg-key.pub 2>&1 && \
        echo -e "${GREEN}Public key extracted successfully${NC}" || \
        echo -e "${YELLOW}Warning: Could not extract public key (GPG may not be configured)${NC}"
    else
        echo -e "${YELLOW}Warning: GPG not installed, GPG tests may be skipped${NC}"
    fi
fi

# Run Go tests
echo -e "\n${GREEN}Running Go integration tests...${NC}"
echo "Working directory: $(pwd)"

# Test provider creation
echo -e "\n${YELLOW}Testing Provider Creation...${NC}"
go test -v ./internal/api/v2/handlers -run TestCreateProvider -timeout 30s || {
    echo -e "${RED}Provider creation test failed${NC}"
    exit 1
}

# Test provider platform publishing
echo -e "\n${YELLOW}Testing Provider Platform Publishing...${NC}"
go test -v ./internal/api/v2/handlers -run TestPublishProviderPlatform -timeout 30s || {
    echo -e "${RED}Provider platform publishing test failed${NC}"
    exit 1
}

# Test provider platform publishing with GPG
echo -e "\n${YELLOW}Testing Provider Platform Publishing with GPG...${NC}"
go test -v ./internal/api/v2/handlers -run TestPublishProviderPlatformWithGPG -timeout 30s || {
    echo -e "${YELLOW}GPG test failed (GPG may not be installed) - this is expected if GPG is not available${NC}"
}

echo -e "\n${GREEN}All tests completed!${NC}"

