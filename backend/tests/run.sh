# Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

export TEST_DATABASE_URL="postgres://iac:iac_password@localhost/iac_platform?sslmode=disable"

go test -v ./internal/api/v2/handlers -run TestListModules
go test -v ./internal/api/v2/handlers -run TestGetModuleVersions
go test -v ./internal/api/v2/handlers -run TestPublishModuleVersion