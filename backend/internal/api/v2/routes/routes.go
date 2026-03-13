// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package routes

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/api/middleware"
	"github.com/iac-platform/backend/internal/api/v2/handlers"
	terraformHandlers "github.com/iac-platform/backend/internal/api/v2/handlers/terraform"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/activity"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/logbuffer"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/registry"
	"github.com/iac-platform/backend/internal/services/state"
	"github.com/iac-platform/backend/internal/services/variable"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/iac-platform/backend/pkg/crypto"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

func SetupV2Routes(
	r *gin.Engine,
	db *gorm.DB,
	authService *auth.Service,
	githubAppManager *vcs.GitHubAppManager,
) {
	// OIDC Discovery Endpoints (unauthenticated — Azure AD calls these to verify tokens)
	// These must be on the root router, not under /api/v2/, and without auth middleware
	oidcSigningKey, err := oidc.NewSigningKey()
	if err != nil {
		logger.Fatalf("Failed to initialize OIDC signing key: %v", err)
	}
	oidcWellKnownHandler := handlers.NewOIDCWellKnownHandler(oidcSigningKey)
	r.GET("/.well-known/openid-configuration", oidcWellKnownHandler.OpenIDConfiguration)
	r.GET("/.well-known/jwks", oidcWellKnownHandler.JWKS)

	// API v2
	v2 := r.Group("/api/v2")
	v2.Use(middleware.AuthMiddleware(authService))

	// VCS Provider Registry (multi-provider support)
	azureDevOpsManager, err := vcs.NewAzureDevOpsManager()
	if err != nil {
		logger.Warnf("Failed to initialize Azure DevOps manager: %v (ADO features will be disabled)", err)
		azureDevOpsManager = nil
	}
	if azureDevOpsManager != nil && azureDevOpsManager.IsEnabled() {
		logger.Infof("Azure DevOps VCS integration enabled (client_id=%s)", azureDevOpsManager.GetClientID())
	} else {
		logger.Infof("Azure DevOps VCS integration disabled (AZURE_DEVOPS_CLIENT_ID/SECRET not set)")
	}
	vcsRegistry := vcs.NewProviderRegistry(githubAppManager, azureDevOpsManager, func(conn *models.VCSConnection) error {
		return repository.NewVCSConnectionRepository(db).Update(conn)
	})

	// Repositories
	orgRepo := repository.NewOrganizationRepository(db)
	projectRepo := repository.NewProjectRepository(db)
	workspaceRepo := repository.NewWorkspaceRepository(db)
	runRepo := repository.NewRunRepository(db)
	vcsConnectionRepo := repository.NewVCSConnectionRepository(db)

	// Activity Service (for activity tracking)
	auditLogRepo := repository.NewAuditLogRepository(db)
	activityService := activity.NewService(auditLogRepo)

	// Team repository (needed for RBAC service)
	teamRepo := repository.NewTeamRepository(db)

	// RBAC Service (with team support)
	rbacService := rbac.NewServiceWithTeams(orgRepo, teamRepo, projectRepo)

	// Handlers
	orgHandler := handlers.NewOrganizationHandlerV2(orgRepo, teamRepo, projectRepo, authService, activityService, rbacService)
	projectHandler := handlers.NewProjectHandlerV2(projectRepo, orgRepo, teamRepo, authService, activityService, rbacService)
	teamHandler := handlers.NewTeamHandlerV2(teamRepo, orgRepo, authService, rbacService)
	teamWorkspaceAccessHandler := handlers.NewTeamWorkspaceAccessHandlerV2(teamRepo, workspaceRepo, projectRepo, orgRepo, authService, rbacService)
	teamProjectAccessHandler := handlers.NewTeamProjectAccessHandlerV2(teamRepo, projectRepo, orgRepo, authService, rbacService)
	agentPoolRepo := repository.NewAgentPoolRepository(db)
	workspaceHandler := terraformHandlers.NewWorkspaceHandlerV2(workspaceRepo, projectRepo, orgRepo, vcsConnectionRepo, teamRepo, agentPoolRepo, authService, activityService, rbacService, vcsRegistry, db)

	// User repository for organization memberships
	userRepo := repository.NewUserRepository(db)
	orgMembershipHandler := handlers.NewOrganizationMembershipHandlerV2(orgRepo, userRepo, teamRepo, authService, rbacService)

	// Declare run handler variable - will be initialized after storage is set up
	var runHandler *terraformHandlers.RunHandlerV2

	// Ping endpoint (TFE-compatible)
	// Note: TFE System API uses /api/v1/ping, but Terraform remote backend may call /api/v2/ping
	v2.GET("/ping", handlers.Ping)

	// Organizations
	orgs := v2.Group("/organizations")
	{
		orgs.GET("", orgHandler.List)
		orgs.POST("", orgHandler.Create)
		orgs.GET("/:name", orgHandler.Get)
		orgs.PATCH("/:name", orgHandler.Update)
		orgs.DELETE("/:name", orgHandler.Delete)
		// TFE-compatible entitlement-set endpoint
		orgs.GET("/:name/entitlement-set", orgHandler.GetEntitlementSet)

		// Organization Memberships (TFE-compatible)
		// TFE uses /api/v2/organizations/:organization/organization-memberships
		// Reference: go-tfe/organization_membership.go - OrganizationMemberships.List/Create
		// Note: Using :name instead of :organization to match existing route pattern
		orgs.GET("/:name/organization-memberships", orgMembershipHandler.List)
		orgs.POST("/:name/organization-memberships", orgMembershipHandler.Create)
	}

	// Organization Memberships by ID (TFE-compatible)
	// TFE uses /api/v2/organization-memberships/:id for Get/Update/Delete
	// Reference: go-tfe/organization_membership.go - OrganizationMemberships.Read/Update/Delete
	orgMemberships := v2.Group("/organization-memberships")
	{
		orgMemberships.GET("/:id", orgMembershipHandler.GetByID)
		orgMemberships.PATCH("/:id", orgMembershipHandler.Update)
		orgMemberships.DELETE("/:id", orgMembershipHandler.Delete)
	}

	// Teams (nested under organizations)
	teams := v2.Group("/organizations/:name/teams")
	{
		teams.GET("", teamHandler.List)
		teams.POST("", teamHandler.Create)
		teams.GET("/:teamName", teamHandler.Get)
		teams.PATCH("/:teamName", teamHandler.Update)
		teams.DELETE("/:teamName", teamHandler.Delete)
	}

	// Teams by ID (TFE-compatible - provider reads teams by ID after creation)
	teamsById := v2.Group("/teams")
	{
		teamsById.GET("/:id", teamHandler.GetByID)
		teamsById.PATCH("/:id", teamHandler.UpdateByID)
		teamsById.DELETE("/:id", teamHandler.DeleteByID)

		// Team Organization Memberships (TFE-compatible)
		// TFE uses /api/v2/teams/:id/relationships/organization-memberships
		// Reference: go-tfe/team_member.go - TeamMembers.Add with OrganizationMembershipIDs
		teamMemberHandler := handlers.NewTeamMemberHandlerV2(teamRepo, orgRepo, userRepo, authService)
		// GET endpoint for listing organization memberships (frontend-specific)
		teamsById.GET("/:id/relationships/organization-memberships", teamMemberHandler.ListOrganizationMemberships)
		teamsById.POST("/:id/relationships/organization-memberships", teamMemberHandler.AddOrganizationMemberships)
		teamsById.DELETE("/:id/relationships/organization-memberships", teamMemberHandler.RemoveOrganizationMemberships)
	}

	// Projects (nested under organizations)
	projects := v2.Group("/organizations/:name/projects")
	{
		projects.GET("", projectHandler.List)
		projects.POST("", projectHandler.Create)
		projects.GET("/:project_name", projectHandler.Get)
		projects.PATCH("/:project_name", projectHandler.Update)
		projects.DELETE("/:project_name", projectHandler.Delete)
	}

	// Projects by ID (TFE-compatible - provider reads projects by ID after creation)
	// TFE-compatible: GET /api/v2/projects/:id (for go-tfe Read)
	v2.GET("/projects/:id", projectHandler.GetByID)
	// TFE-compatible: DELETE /api/v2/projects/:id (for go-tfe Delete)
	v2.DELETE("/projects/:id", projectHandler.DeleteByID)

	// Agent Pools (TFE-compatible)
	// Reference: go-tfe/agent_pool.go, terraform-provider-tfe agent_pool resources
	runnerRepo := repository.NewRunnerRepository(db)
	agentPoolHandler := handlers.NewAgentPoolHandlerV2(agentPoolRepo, runnerRepo, orgRepo, authService, rbacService)
	orgAgentPools := v2.Group("/organizations/:name/agent-pools")
	{
		orgAgentPools.GET("", agentPoolHandler.List)
		orgAgentPools.POST("", agentPoolHandler.Create)
	}
	agentPools := v2.Group("/agent-pools")
	{
		agentPools.GET("/:id/agents", agentPoolHandler.ListAgents)
		agentPools.GET("/:id", agentPoolHandler.GetByID)
		agentPools.PATCH("/:id", agentPoolHandler.Update)
		agentPools.DELETE("/:id", agentPoolHandler.Delete)
	}

	// Azure OIDC Configurations (TFE-compatible)
	// Reference: go-tfe/azure_oidc_configuration.go
	azureOIDCRepo := repository.NewAzureOIDCConfigurationRepository(db)
	azureOIDCHandler := handlers.NewAzureOIDCConfigurationHandlerV2(azureOIDCRepo, orgRepo, authService, rbacService)
	// Create: org-scoped
	v2.POST("/organizations/:name/oidc-configurations", azureOIDCHandler.Create)
	// List: org-scoped
	v2.GET("/organizations/:name/oidc-configurations", azureOIDCHandler.List)
	// Read/Update/Delete: by ID (shared OIDCConfigPathFormat across Azure/AWS/GCP/Vault)
	oidcConfigs := v2.Group("/oidc-configurations")
	{
		oidcConfigs.GET("/:id", azureOIDCHandler.Read)
		oidcConfigs.PATCH("/:id", azureOIDCHandler.Update)
		oidcConfigs.DELETE("/:id", azureOIDCHandler.Delete)
	}

	// Admin: Terraform Versions (TFE-compatible)
	// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/admin/terraform-versions
	adminTFVersionHandler := handlers.NewAdminTerraformVersionsHandler(db, authService)
	adminTFVersions := v2.Group("/admin/terraform-versions")
	{
		adminTFVersions.GET("", adminTFVersionHandler.List)
		adminTFVersions.POST("", adminTFVersionHandler.Create)
		adminTFVersions.GET("/:id", adminTFVersionHandler.Read)
		adminTFVersions.PATCH("/:id", adminTFVersionHandler.Update)
		adminTFVersions.DELETE("/:id", adminTFVersionHandler.Delete)
	}

	// Public: List enabled Terraform versions (any authenticated user)
	// Used by workspace create/edit to populate the version dropdown
	v2.GET("/terraform-versions", adminTFVersionHandler.ListEnabled)

	// Runners (Self-Hosted Runners Management)
	// Reference: SELF_HOSTED_RUNNERS_DESIGN.md
	runnerJobExecRepo := repository.NewRunnerJobExecutionRepository(db)
	runnerHandler := handlers.NewRunnerHandlerV2(runnerRepo, runnerJobExecRepo, agentPoolRepo, orgRepo, rbacService)

	// Runners by organization (management API)
	orgRunners := v2.Group("/organizations/:name/runners")
	{
		orgRunners.GET("", runnerHandler.List)
		orgRunners.GET("/stats", runnerHandler.GetStats)
	}

	// Runners by ID
	runners := v2.Group("/runners")
	{
		runners.GET("/:id", runnerHandler.GetByID)
		runners.PATCH("/:id", runnerHandler.Update)
		runners.DELETE("/:id", runnerHandler.Delete)
	}

	// Ansible Config (ansible.cfg management at org/project scope)
	ansibleConfigHandlerRepo := repository.NewAnsibleConfigRepository(db)
	ansibleConfigHandler := handlers.NewAnsibleConfigHandler(ansibleConfigHandlerRepo, orgRepo, projectRepo, rbacService, db)

	// Organization-level ansible config
	v2.GET("/organizations/:name/ansible-config", ansibleConfigHandler.GetByOrganization)
	v2.PUT("/organizations/:name/ansible-config", ansibleConfigHandler.UpsertByOrganization)
	v2.DELETE("/organizations/:name/ansible-config", ansibleConfigHandler.DeleteByOrganization)
	v2.GET("/organizations/:name/ansible-config/effective", ansibleConfigHandler.GetEffective)

	// Project-level ansible config
	v2.GET("/projects/:id/ansible-config", ansibleConfigHandler.GetByProject)
	v2.PUT("/projects/:id/ansible-config", ansibleConfigHandler.UpsertByProject)
	v2.DELETE("/projects/:id/ansible-config", ansibleConfigHandler.DeleteByProject)

	// Webhook Events (for debugging webhook deliveries)
	webhookEventRepo := repository.NewWebhookEventRepository(db)
	webhookEventHandler := handlers.NewWebhookEventHandlerV2(webhookEventRepo, orgRepo)
	orgWebhookEvents := v2.Group("/organizations/:name/webhook-events")
	{
		orgWebhookEvents.GET("", webhookEventHandler.List)
	}

	// Terraform Workspaces (TFE-compatible)
	// TFE expects: /api/v2/organizations/:name/workspaces/:name
	tfWorkspaces := v2.Group("/organizations/:name/workspaces")
	{
		tfWorkspaces.GET("", workspaceHandler.ListByOrganization)
		tfWorkspaces.POST("", workspaceHandler.Create)
		tfWorkspaces.GET("/:workspace_name", workspaceHandler.GetByOrganizationAndName)
		tfWorkspaces.PATCH("/:workspace_name", workspaceHandler.Update)
		tfWorkspaces.DELETE("/:workspace_name", workspaceHandler.Delete)
		tfWorkspaces.POST("/:workspace_name/actions/safe-delete", workspaceHandler.SafeDelete)
	}

	// Terraform Workspaces (internal API using UUIDs)
	tfWorkspacesInternal := v2.Group("/terraform/workspaces")
	{
		tfWorkspacesInternal.GET("/:id", workspaceHandler.GetByID)
	}

	// TFE-compatible: GET /api/v2/workspaces/:id (for go-tfe ReadByID)
	// This endpoint is required by tfe_team_access and other resources that need to read workspaces by ID
	v2.GET("/workspaces/:id", workspaceHandler.GetByID)
	// TFE-compatible: PATCH /api/v2/workspaces/:id (for go-tfe UpdateByID, used by tfe_workspace_settings)
	v2.PATCH("/workspaces/:id", workspaceHandler.UpdateByID)

	// TFE-compatible: DELETE /api/v2/workspaces/:id (force delete by ID)
	v2.DELETE("/workspaces/:id", workspaceHandler.DeleteByID)

	// Workspace actions (using workspace ID - must use :id to match other workspace routes)
	workspaceActions := v2.Group("/workspaces/:id")
	{
		workspaceActions.POST("/actions/lock", workspaceHandler.Lock)
		workspaceActions.POST("/actions/unlock", workspaceHandler.Unlock)
		workspaceActions.POST("/actions/force-unlock", workspaceHandler.ForceUnlock)
		workspaceActions.POST("/actions/safe-delete", workspaceHandler.SafeDeleteByID)
	}

	// Team Workspace Access (TFE-compatible)
	// TFE uses /api/v2/team-workspaces (not workspace-scoped)
	// Reference: go-tfe/team_access.go - TeamAccess.Add uses "team-workspaces"
	teamWorkspaceAccess := v2.Group("/team-workspaces")
	{
		teamWorkspaceAccess.GET("", teamWorkspaceAccessHandler.List)          // With filter[workspace][id] query param
		teamWorkspaceAccess.POST("", teamWorkspaceAccessHandler.Create)       // Team and workspace in relationships
		teamWorkspaceAccess.GET("/:id", teamWorkspaceAccessHandler.Get)       // Reuse Get method (it accepts ID in URL)
		teamWorkspaceAccess.PATCH("/:id", teamWorkspaceAccessHandler.Update)  // Reuse Update method (it accepts ID in URL)
		teamWorkspaceAccess.DELETE("/:id", teamWorkspaceAccessHandler.Delete) // Reuse Delete method (it accepts ID in URL)
	}

	// Legacy workspace-scoped endpoints (for backward compatibility if needed)
	// These are NOT used by go-tfe, but kept for potential future use
	teamWorkspaceAccessLegacy := v2.Group("/workspaces/:id/relationships/team-access")
	{
		teamWorkspaceAccessLegacy.GET("", teamWorkspaceAccessHandler.List)
		teamWorkspaceAccessLegacy.POST("", teamWorkspaceAccessHandler.Create)
		teamWorkspaceAccessLegacy.GET("/:access_id", teamWorkspaceAccessHandler.Get)
		teamWorkspaceAccessLegacy.PATCH("/:access_id", teamWorkspaceAccessHandler.Update)
		teamWorkspaceAccessLegacy.DELETE("/:access_id", teamWorkspaceAccessHandler.Delete)
	}

	// Team Project Access (TFE-compatible)
	// TFE uses /api/v2/team-projects (not project-scoped)
	// Reference: go-tfe/team_project_access.go - TeamProjectAccesses.Add uses "team-projects"
	teamProjectAccess := v2.Group("/team-projects")
	{
		teamProjectAccess.GET("", teamProjectAccessHandler.List)          // With filter[project][id] query param
		teamProjectAccess.POST("", teamProjectAccessHandler.Create)       // Team and project in relationships
		teamProjectAccess.GET("/:id", teamProjectAccessHandler.Get)       // Reuse Get method (it accepts ID in URL)
		teamProjectAccess.PATCH("/:id", teamProjectAccessHandler.Update)  // Reuse Update method (it accepts ID in URL)
		teamProjectAccess.DELETE("/:id", teamProjectAccessHandler.Delete) // Reuse Delete method (it accepts ID in URL)
	}

	// Legacy project-scoped endpoints (for backward compatibility if needed)
	// These are NOT used by go-tfe, but kept for potential future use
	teamProjectAccessLegacy := v2.Group("/projects/:id/relationships/team-access")
	{
		teamProjectAccessLegacy.GET("", teamProjectAccessHandler.List)
		teamProjectAccessLegacy.POST("", teamProjectAccessHandler.Create)
		teamProjectAccessLegacy.GET("/:access_id", teamProjectAccessHandler.Get)
		teamProjectAccessLegacy.PATCH("/:access_id", teamProjectAccessHandler.Update)
		teamProjectAccessLegacy.DELETE("/:access_id", teamProjectAccessHandler.Delete)
	}

	// Configuration Versions Repository and Handler
	configVersionRepo := repository.NewConfigurationVersionRepository(db)

	// Initialize storage for configuration versions (same as registry storage)
	storageBackend := os.Getenv("STORAGE_BACKEND")
	if storageBackend == "" {
		storageBackend = "minio"
	}

	var configStorageClient storage.Client
	var configStorageBucket string

	if storageBackend == "minio" {
		minioEndpoint := os.Getenv("MINIO_ENDPOINT")
		if minioEndpoint == "" {
			minioEndpoint = "localhost:9000"
		}
		minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
		if minioAccessKey == "" {
			minioAccessKey = "minioadmin"
		}
		minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
		if minioSecretKey == "" {
			minioSecretKey = "minioadmin"
		}
		configStorageBucket = os.Getenv("STORAGE_BUCKET")
		if configStorageBucket == "" {
			configStorageBucket = "terraform-registry"
		}
		useSSL := os.Getenv("MINIO_USE_SSL") == "true"

		var err error
		configStorageClient, err = storage.NewMinIOClient(minioEndpoint, minioAccessKey, minioSecretKey, configStorageBucket, useSSL)
		if err != nil {
			logger.Warnf("Failed to initialize MinIO storage for configuration versions: %v (uploads will fail)", err)
		}
	}

	configVersionHandler := terraformHandlers.NewConfigurationVersionHandlerV2(configVersionRepo, workspaceRepo, authService, configStorageClient, configStorageBucket)

	// Initialize Redis log buffer service for log streaming
	// Initialize Redis connection for log streaming (reuse same config as Ansible queue)
	var logBufferService *logbuffer.RedisLogBuffer
	redisLogHost := os.Getenv("REDIS_HOST")
	if redisLogHost == "" {
		redisLogHost = "localhost"
	}
	redisLogPort := 6379
	if portStr := os.Getenv("REDIS_PORT"); portStr != "" {
		if p, parseErr := parsePort(portStr); parseErr == nil {
			redisLogPort = p
		}
	}
	redisLogPassword := os.Getenv("REDIS_PASSWORD")
	redisLogQueue, logQueueErr := queue.NewRedisQueue(redisLogHost, redisLogPort, redisLogPassword, 0)
	if logQueueErr != nil {
		logger.Warnf("Failed to initialize Redis queue for log streaming: %v (log streaming will fall back to MinIO)", logQueueErr)
	} else {
		logBufferService = logbuffer.NewRedisLogBuffer(redisLogQueue.Client())
	}

	// Initialize run handler with storage client (for logs endpoint)
	// Use the same storage client as configuration versions
	// Add VCS connection repository and GitHub app manager for automatic configuration version creation from VCS
	phaseStateRepo := repository.NewRunPhaseStateRepository(db)
	stateVersionRepo := repository.NewStateVersionRepository(db)
	runHandler = terraformHandlers.NewRunHandlerV2(runRepo, workspaceRepo, orgRepo, authService, configStorageClient, configVersionRepo, vcsConnectionRepo, vcsRegistry, logBufferService, phaseStateRepo, rbacService, stateVersionRepo)

	// Terraform Runs (TFE-compatible)
	// TFE expects: /api/v2/runs/:id
	tfRuns := v2.Group("/runs")
	{
		tfRuns.POST("", runHandler.Create)
		tfRuns.GET("/:id", runHandler.Get)
		tfRuns.GET("/:id/plan", runHandler.GetPlan)
		tfRuns.GET("/:id/outputs", runHandler.GetOutputs)
		tfRuns.GET("/:id/logs", runHandler.GetLogs)            // Generic endpoint (backward compatible)
		tfRuns.GET("/:id/logs/plan", runHandler.GetPlanLogs)   // Explicit plan logs endpoint
		tfRuns.GET("/:id/logs/apply", runHandler.GetApplyLogs) // Explicit apply logs endpoint
		tfRuns.POST("/:id/actions/apply", runHandler.Apply)
		tfRuns.POST("/:id/actions/cancel", runHandler.Cancel)
		tfRuns.POST("/:id/actions/discard", runHandler.Discard)
		tfRuns.POST("/:id/actions/force-cancel", runHandler.ForceCancel)
		tfRuns.POST("/:id/actions/force-execute", runHandler.ForceExecute)
	}

	// Plans endpoint (TFE-compatible)
	// TFE supports both /api/v2/runs/:id/plan AND /api/v2/plans/:id
	// For plan operations, plan ID = run ID
	v2.GET("/plans/:id", runHandler.GetPlan)

	// Applies endpoint (TFE-compatible)
	// TFE expects: /api/v2/applies/:id
	// For plan-and-apply runs, apply ID = run ID
	v2.GET("/applies/:id", runHandler.GetApply)

	// Workspace Runs (list runs by workspace ID)
	workspaceRuns := v2.Group("/workspaces/:id/runs")
	{
		workspaceRuns.GET("", runHandler.ListByWorkspace)
	}

	// Organization Runs (TFE-compatible)
	// TFE expects: /api/v2/organizations/:name/runs
	orgRuns := v2.Group("/organizations/:name/runs")
	{
		orgRuns.GET("", runHandler.ListByOrganization)
		orgRuns.GET("/queue", runHandler.GetQueue)
	}

	// Configuration Versions (TFE-compatible)
	// TFE expects: /api/v2/workspaces/:id/configuration-versions
	workspaceConfigVersions := v2.Group("/workspaces/:id/configuration-versions")
	{
		workspaceConfigVersions.GET("", configVersionHandler.ListByWorkspace)
		workspaceConfigVersions.POST("", configVersionHandler.Create)
	}

	// Configuration Versions by ID (TFE-compatible)
	// TFE expects: /api/v2/configuration-versions/:id
	configVersionsById := v2.Group("/configuration-versions")
	{
		configVersionsById.GET("/:id", configVersionHandler.Get)
	}

	// Upload endpoint (no auth middleware - uses token in query parameter)
	// This must be registered separately because it uses token-based auth, not Authorization header
	uploadEndpoint := r.Group("/api/v2/configuration-versions")
	uploadEndpoint.PUT("/:id/upload", configVersionHandler.Upload)

	// Repositories for state versions, variables, and tokens
	// stateVersionRepo created earlier for runHandler
	stateLockRepo := repository.NewStateLockRepository(db)
	variableRepo := repository.NewVariableRepository(db)
	tfeTokenRepo := repository.NewTFETokenRepository(db)

	// State Service (for lock checking and state management)
	// Use same storage client as configuration versions (state stored in same bucket, different paths)
	stateService := state.NewService(stateVersionRepo, stateLockRepo, workspaceRepo, configStorageClient)

	// State Versions Handlers (reuse same storage client as configuration versions)
	stateVersionHandler := terraformHandlers.NewStateVersionHandlerV2(stateVersionRepo, workspaceRepo, projectRepo, authService, rbacService, stateService, configStorageClient, configStorageBucket)

	// State Versions (TFE-compatible)
	// TFE expects: /api/v2/workspaces/:id/state-versions
	// IMPORTANT: Register remove-resource BEFORE the group to ensure proper route matching
	v2.POST("/workspaces/:id/state-versions/remove-resource", stateVersionHandler.RemoveResource)
	// TFE: GET /workspaces/:id/current-state-version — latest state version + hosted-state-download-url (fixes tfe_* drift)
	v2.GET("/workspaces/:id/current-state-version", stateVersionHandler.CurrentStateVersion)

	stateVersions := v2.Group("/workspaces/:id/state-versions")
	{
		stateVersions.GET("", stateVersionHandler.ListByWorkspace)
		stateVersions.POST("", stateVersionHandler.Create)
	}

	// State Versions by ID (TFE-compatible)
	// TFE expects: /api/v2/state-versions/:id
	stateVersionsById := v2.Group("/state-versions")
	{
		stateVersionsById.GET("/:id", stateVersionHandler.Get)
		stateVersionsById.GET("/:id/download", stateVersionHandler.Download) // TFE hosted-state-download-url target
		stateVersionsById.GET("/:id/outputs", stateVersionHandler.GetOutputs)
		// Delete state version (StackWeaver-specific feature)
		stateVersionsById.DELETE("/:id", stateVersionHandler.Delete)
	}

	// Get encryption key for variables (reuse same logic as Ansible)
	variableEncryptionKey := os.Getenv("ENCRYPTION_KEY")
	var variableEncryptionKeyBytes []byte
	if variableEncryptionKey != "" {
		var decodeErr error
		variableEncryptionKeyBytes, decodeErr = hex.DecodeString(variableEncryptionKey)
		if decodeErr != nil {
			logger.Warnf("Failed to decode variable encryption key: %v (using raw bytes)", decodeErr)
			variableEncryptionKeyBytes = []byte(variableEncryptionKey)
		}
		// Ensure key is 32 bytes for AES-256
		if len(variableEncryptionKeyBytes) < 32 {
			paddedKey := make([]byte, 32)
			copy(paddedKey, variableEncryptionKeyBytes)
			variableEncryptionKeyBytes = paddedKey
		} else if len(variableEncryptionKeyBytes) > 32 {
			variableEncryptionKeyBytes = variableEncryptionKeyBytes[:32]
		}
	} else {
		logger.Warn("No encryption key configured for variables (set ENCRYPTION_KEY)")
		// Use a default key for development only - this should be overridden in production
		variableEncryptionKeyBytes = make([]byte, 32)
	}
	// Create variable service with workspace support for platform variables
	// Note: variableSetRepo is created later, but we can still use workspace repo for platform vars
	variableService := variable.NewServiceWithVariableSetsAndWorkspace(variableRepo, nil, workspaceRepo, variableEncryptionKeyBytes)

	// Variables Handlers
	variableHandler := terraformHandlers.NewVariableHandlerV2(variableRepo, workspaceRepo, authService, rbacService, variableService)
	// Set repositories for building TFE-compatible links
	variableHandler.SetRepositories(orgRepo, projectRepo)

	// Variables (TFE-compatible)
	// TFE spec: /api/v2/workspaces/:workspace_id/vars
	// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/workspace-variables
	variables := v2.Group("/workspaces/:id/vars")
	{
		variables.GET("", variableHandler.ListByWorkspace)
		variables.GET("/:variable_id", variableHandler.Get) // TFE: Show variable — provider Read/refresh
		variables.POST("", variableHandler.Create)
		variables.PATCH("/:variable_id", variableHandler.Update)
		variables.DELETE("/:variable_id", variableHandler.Delete)
	}

	// Platform Variables (StackWeaver-specific endpoint for frontend warnings)
	// GET /api/v2/workspaces/:id/platform-variables
	v2.GET("/workspaces/:id/platform-variables", variableHandler.GetPlatformVariableKeys)

	// Variable Sets Handlers
	variableSetRepo := repository.NewVariableSetRepository(db)
	variableSetVariableRepo := repository.NewVariableSetVariableRepository(db)
	jobTemplateRepo := repository.NewAnsibleJobTemplateRepository(db)
	// projectRepo already declared above, reuse it
	variableSetHandler := handlers.NewVariableSetHandlerV2(variableSetRepo, variableSetVariableRepo, orgRepo, projectRepo, workspaceRepo, jobTemplateRepo, authService)

	// Update variable service to include variable set repo (for variable set support in GetVariablesForRun)
	// This allows platform variables + variable sets to work together
	variableService = variable.NewServiceWithVariableSetsAndWorkspace(variableRepo, variableSetRepo, workspaceRepo, variableEncryptionKeyBytes)
	// Update the handler with the new service
	variableHandler = terraformHandlers.NewVariableHandlerV2(variableRepo, workspaceRepo, authService, rbacService, variableService)
	variableHandler.SetRepositories(orgRepo, projectRepo)

	// Variable Sets (TFE-compatible)
	// TFE spec: /api/v2/organizations/:organization_name/varsets
	// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets
	variableSets := v2.Group("/organizations/:name/varsets")
	{
		variableSets.GET("", variableSetHandler.ListVariableSets)
		variableSets.POST("", variableSetHandler.CreateVariableSet)
		variableSets.GET("/:id", variableSetHandler.GetVariableSet)
		variableSets.PATCH("/:id", variableSetHandler.UpdateVariableSet)
		variableSets.DELETE("/:id", variableSetHandler.DeleteVariableSet)

		// TFE spec: /api/v2/varsets/:varset_id/relationships/workspaces
		variableSets.POST("/:id/relationships/workspaces", variableSetHandler.AssignWorkspace)
		variableSets.DELETE("/:id/relationships/workspaces", variableSetHandler.UnassignWorkspace)
		variableSets.POST("/:id/relationships/projects", variableSetHandler.AssignProject)
		variableSets.DELETE("/:id/relationships/projects", variableSetHandler.UnassignProject)
		// Note: Job templates inherit variable sets from projects automatically (TFE-compatible)
		// No explicit assignment endpoints needed - managed via project assignment

		// TFE spec: /api/v2/varsets/:varset_id/relationships/vars
		variableSetVars := variableSets.Group("/:id/relationships/vars")
		{
			variableSetVars.GET("", variableSetHandler.ListVariableSetVariables)
			variableSetVars.GET("/:variable_id", variableSetHandler.GetVariableSetVariable) // TFE: Show var — provider Read/refresh
			variableSetVars.POST("", variableSetHandler.CreateVariableSetVariable)
			variableSetVars.PATCH("/:variable_id", variableSetHandler.UpdateVariableSetVariable)
			variableSetVars.DELETE("/:variable_id", variableSetHandler.DeleteVariableSetVariable)
		}
	}

	// TFE spec: /api/v2/varsets/:varset_id (direct access without organization prefix)
	varsetsById := v2.Group("/varsets")
	{
		varsetsById.GET("/:id", variableSetHandler.GetVariableSet)
		varsetsById.PATCH("/:id", variableSetHandler.UpdateVariableSet)
		varsetsById.DELETE("/:id", variableSetHandler.DeleteVariableSet)
		varsetsById.POST("/:id/relationships/workspaces", variableSetHandler.AssignWorkspace)
		varsetsById.DELETE("/:id/relationships/workspaces", variableSetHandler.UnassignWorkspace)
		varsetsById.POST("/:id/relationships/projects", variableSetHandler.AssignProject)
		varsetsById.DELETE("/:id/relationships/projects", variableSetHandler.UnassignProject)
		// Note: Job templates inherit variable sets from projects automatically (TFE-compatible)
		varsetsById.GET("/:id/relationships/vars", variableSetHandler.ListVariableSetVariables)
		varsetsById.GET("/:id/relationships/vars/:variable_id", variableSetHandler.GetVariableSetVariable) // TFE: Show var — provider Read/refresh
		varsetsById.POST("/:id/relationships/vars", variableSetHandler.CreateVariableSetVariable)
		varsetsById.PATCH("/:id/relationships/vars/:variable_id", variableSetHandler.UpdateVariableSetVariable)
		varsetsById.DELETE("/:id/relationships/vars/:variable_id", variableSetHandler.DeleteVariableSetVariable)
	}

	// TFE Token Management
	tokenHandler := handlers.NewTokenHandlerV2(tfeTokenRepo, authService)
	tokens := v2.Group("/tokens")
	{
		tokens.GET("", tokenHandler.List)
		tokens.POST("", tokenHandler.Create)
		tokens.DELETE("/:id", tokenHandler.Delete)
	}

	// VCS Connections
	vcsConnectionHandler := handlers.NewVCSConnectionHandlerV2(vcsConnectionRepo, orgRepo, authService, vcsRegistry, rbacService)

	// Initialize Registry Components (needed for webhook handler and publishing routes)
	moduleRepo := repository.NewModuleRepository(db)
	moduleVersionRepo := repository.NewModuleVersionRepository(db)
	moduleDownloadRepo := repository.NewModuleDownloadRepository(db)

	// Initialize storage for registry (reuse same config as configuration versions)
	var registryStorage registry.StorageBackend
	var storageBucket string

	if storageBackend == "minio" {
		minioEndpoint := os.Getenv("MINIO_ENDPOINT")
		if minioEndpoint == "" {
			minioEndpoint = "localhost:9000"
		}
		minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
		if minioAccessKey == "" {
			minioAccessKey = "minioadmin"
		}
		minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
		if minioSecretKey == "" {
			minioSecretKey = "minioadmin"
		}
		storageBucket = os.Getenv("STORAGE_BUCKET")
		if storageBucket == "" {
			storageBucket = "terraform-registry"
		}
		useSSL := os.Getenv("MINIO_USE_SSL") == "true"

		var err error
		registryStorage, err = registry.NewMinIOStorage(minioEndpoint, minioAccessKey, minioSecretKey, storageBucket, useSSL)
		if err != nil {
			logger.Warnf("Failed to initialize MinIO storage for registry: %v (registry features will be limited)", err)
		}
	}

	// Module Publisher
	modulePublisher := registry.NewModulePublisher(
		moduleRepo,
		moduleVersionRepo,
		orgRepo,
		vcsConnectionRepo,
		registryStorage,
		storageBucket,
	)

	// Registry Publishing Handler
	registryPublishingHandler := handlers.NewRegistryPublishingHandler(
		moduleRepo,
		moduleVersionRepo,
		orgRepo,
		vcsConnectionRepo,
		authService,
		githubAppManager,
		modulePublisher,
	)

	// VCS Connections (organization-scoped)
	orgVCSConnections := v2.Group("/organizations/:name/vcs-connections")
	{
		orgVCSConnections.GET("", vcsConnectionHandler.List)
		orgVCSConnections.POST("", vcsConnectionHandler.Create)
		// GitHub App installation initiation routes are added later
	}

	// VCS Connections (by ID)
	vcsConnections := v2.Group("/vcs-connections")
	{
		vcsConnections.GET("/:id", vcsConnectionHandler.Get)
		vcsConnections.DELETE("/:id", vcsConnectionHandler.Delete)
		// Repository and branch listing
		vcsConnections.GET("/:id/repositories", vcsConnectionHandler.ListRepositories)
		vcsConnections.GET("/:id/repositories/:owner/:repo/branches", vcsConnectionHandler.ListBranches)
		// File content retrieval
		vcsConnections.GET("/:id/repositories/:owner/:repo/contents/*path", vcsConnectionHandler.GetFileContent)
		// List YAML files
		vcsConnections.GET("/:id/repositories/:owner/:repo/yaml-files", vcsConnectionHandler.ListYamlFiles)
		// List inventory files (.ini, .yaml, .yml, .json)
		vcsConnections.GET("/:id/repositories/:owner/:repo/inventory-files", vcsConnectionHandler.ListInventoryFiles)
	}

	// Registry Publishing Routes (Authenticated)
	orgRegistry := v2.Group("/organizations/:name/registry")
	{
		modules := orgRegistry.Group("/modules")
		{
			modules.POST("", registryPublishingHandler.CreateModule)
			modules.GET("", registryPublishingHandler.ListModules)
			modules.DELETE("", registryPublishingHandler.DeleteAllModules)
			modules.GET("/:module_name/:provider", registryPublishingHandler.GetModule)
			modules.DELETE("/:module_name/:provider", registryPublishingHandler.DeleteModule)
			modules.GET("/:module_name/:provider/versions", registryPublishingHandler.ListModuleVersions)
			modules.POST("/:module_name/:provider/versions", registryPublishingHandler.PublishVersion)
			modules.DELETE("/:module_name/:provider/versions/:version", registryPublishingHandler.DeleteModuleVersion)
		}
	}

	// GitHub App Webhook (public endpoint - GitHub sends webhooks here)
	// This endpoint is outside the authenticated v2 group because GitHub needs to access it
	if githubAppManager != nil && githubAppManager.IsEnabled() {
		vcsWebhook := r.Group("/api/v2/vcs-connections/github")
		{
			// Initialize storage for VCS webhook handler
			// Reuse the same storage client as configuration versions to ensure consistency
			// The webhook handler needs to upload configuration tarballs to the same bucket
			// Use configStorageClient (same instance used by UI-triggered runs)
			// This ensures webhook-triggered runs use the exact same storage configuration
			var storageClient storage.Client

			if configStorageClient != nil {
				// Primary path: reuse configuration versions storage client
				storageClient = configStorageClient
				logger.Infof("VCS webhook handler using configStorageClient (shared with UI-triggered runs), bucket: %s", configStorageBucket)
			} else {
				// Fallback: initialize with EXACT same logic as configStorageClient
				// This should rarely happen, but ensures consistency if configStorageClient failed to initialize
				storageBackend := os.Getenv("STORAGE_BACKEND")
				if storageBackend == "" {
					storageBackend = "minio"
				}
				if storageBackend == "minio" {
					minioEndpoint := os.Getenv("MINIO_ENDPOINT")
					if minioEndpoint == "" {
						minioEndpoint = "localhost:9000"
					}
					minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
					if minioAccessKey == "" {
						minioAccessKey = "minioadmin"
					}
					minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
					if minioSecretKey == "" {
						minioSecretKey = "minioadmin"
					}
					// Use STORAGE_BUCKET (same as configuration versions), not MINIO_BUCKET
					storageBucket := os.Getenv("STORAGE_BUCKET")
					if storageBucket == "" {
						storageBucket = "terraform-registry" // Same default as configuration versions
					}
					useSSL := os.Getenv("MINIO_USE_SSL") == "true"

					var err error
					storageClient, err = storage.NewMinIOClient(minioEndpoint, minioAccessKey, minioSecretKey, storageBucket, useSSL)
					if err != nil {
						logger.Warnf("Failed to initialize MinIO client for VCS webhook: %v", err)
					} else {
						logger.Infof("VCS webhook handler initialized with fallback storage client, bucket: %s", storageBucket)
					}
				}
			}

			// Get Ansible inventory repo for inventory sync
			ansibleInventoryRepoForWebhook := repository.NewAnsibleInventoryRepository(db)

			// Get Ansible playbook repo for playbook sync
			ansiblePlaybookRepoForWebhook := repository.NewAnsiblePlaybookRepository(db)

			// Initialize Ansible Redis queue for inventory sync (reuse same config as SetupAnsibleRoutes)
			redisHost := os.Getenv("REDIS_HOST")
			if redisHost == "" {
				redisHost = "localhost"
			}
			redisPort := 6379
			if portStr := os.Getenv("REDIS_PORT"); portStr != "" {
				if p, err := parsePort(portStr); err == nil {
					redisPort = p
				}
			}
			redisPassword := os.Getenv("REDIS_PASSWORD")

			var ansibleQueueForWebhook queue.Queue
			ansibleQueueForWebhook, err := queue.NewRedisQueue(redisHost, redisPort, redisPassword, 0)
			if err != nil {
				logger.Warnf("Failed to initialize Redis queue for inventory sync: %v", err)
			}

			webhookEventRepoForHandler := repository.NewWebhookEventRepository(db)
			appHandler := handlers.NewVCSAppInstallationHandlerV2(
				vcsConnectionRepo,
				orgRepo,
				moduleRepo,
				workspaceRepo,
				runRepo,
				configVersionRepo,
				ansibleInventoryRepoForWebhook,
				ansiblePlaybookRepoForWebhook,
				webhookEventRepoForHandler,
				authService,
				githubAppManager,
				azureDevOpsManager,
				vcsRegistry,
				registryPublishingHandler,
				storageClient,
				ansibleQueueForWebhook,
			)
			vcsWebhook.POST("/webhook", appHandler.HandleInstallationWebhook)
		}

		// Also update the installation initiation handler
		orgVCSConnections := v2.Group("/organizations/:name/vcs-connections")
		{
			// Initialize storage for VCS connection handlers
			storageBackend := os.Getenv("STORAGE_BACKEND")
			if storageBackend == "" {
				storageBackend = "minio"
			}
			var storageClient storage.Client
			var storageBucket string
			if storageBackend == "minio" {
				minioEndpoint := os.Getenv("MINIO_ENDPOINT")
				minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
				minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
				storageBucket = os.Getenv("MINIO_BUCKET")
				if storageBucket == "" {
					storageBucket = "iac-platform"
				}
				useSSL := os.Getenv("MINIO_USE_SSL") == "true"
				if minioEndpoint != "" && minioAccessKey != "" && minioSecretKey != "" {
					var err error
					storageClient, err = storage.NewMinIOClient(minioEndpoint, minioAccessKey, minioSecretKey, storageBucket, useSSL)
					if err != nil {
						logger.Warnf("Failed to initialize MinIO client for VCS connections: %v", err)
					}
				}
			}

			// Get Ansible inventory repo for inventory sync
			ansibleInventoryRepoForInstall := repository.NewAnsibleInventoryRepository(db)

			// Get Ansible playbook repo for playbook sync
			ansiblePlaybookRepoForInstall := repository.NewAnsiblePlaybookRepository(db)

			// Initialize Ansible Redis queue for inventory sync (reuse same config)
			redisHost := os.Getenv("REDIS_HOST")
			if redisHost == "" {
				redisHost = "localhost"
			}
			redisPort := 6379
			if portStr := os.Getenv("REDIS_PORT"); portStr != "" {
				if p, err := parsePort(portStr); err == nil {
					redisPort = p
				}
			}
			redisPassword := os.Getenv("REDIS_PASSWORD")

			var ansibleQueueForInstall queue.Queue
			ansibleQueueForInstall, err := queue.NewRedisQueue(redisHost, redisPort, redisPassword, 0)
			if err != nil {
				logger.Warnf("Failed to initialize Redis queue for inventory sync: %v", err)
			}

			webhookEventRepoForInstall := repository.NewWebhookEventRepository(db)
			appHandler := handlers.NewVCSAppInstallationHandlerV2(
				vcsConnectionRepo,
				orgRepo,
				moduleRepo,
				workspaceRepo,
				runRepo,
				configVersionRepo,
				ansibleInventoryRepoForInstall,
				ansiblePlaybookRepoForInstall,
				webhookEventRepoForInstall,
				authService,
				githubAppManager,
				azureDevOpsManager,
				vcsRegistry,
				registryPublishingHandler,
				storageClient,
				ansibleQueueForInstall,
			)
			orgVCSConnections.GET("/github/install", appHandler.InitiateInstallation)
			orgVCSConnections.POST("/github/installations/:installation_id", appHandler.CreateConnectionFromInstallation)

			// Azure DevOps OAuth2 installation flow (authenticated)
			orgVCSConnections.GET("/azure-devops/install", appHandler.InitiateAzureDevOpsInstallation)
		}
	}

	// Azure DevOps public endpoints (no auth required)
	{
		adoWebhook := r.Group("/api/v2/vcs-connections/azure-devops")
		{
			// Reuse same repos/services from outer scope
			ansibleInventoryRepoForADO := repository.NewAnsibleInventoryRepository(db)
			ansiblePlaybookRepoForADO := repository.NewAnsiblePlaybookRepository(db)
			webhookEventRepoForADO := repository.NewWebhookEventRepository(db)

			redisHost := os.Getenv("REDIS_HOST")
			if redisHost == "" {
				redisHost = "localhost"
			}
			redisPort := 6379
			if portStr := os.Getenv("REDIS_PORT"); portStr != "" {
				if p, err := parsePort(portStr); err == nil {
					redisPort = p
				}
			}
			redisPassword := os.Getenv("REDIS_PASSWORD")
			var ansibleQueueForADO queue.Queue
			ansibleQueueForADO, adoQueueErr := queue.NewRedisQueue(redisHost, redisPort, redisPassword, 0)
			if adoQueueErr != nil {
				logger.Warnf("Failed to initialize Redis queue for ADO webhook: %v", adoQueueErr)
			}

			adoHandler := handlers.NewVCSAppInstallationHandlerV2(
				vcsConnectionRepo,
				orgRepo,
				moduleRepo,
				workspaceRepo,
				runRepo,
				configVersionRepo,
				ansibleInventoryRepoForADO,
				ansiblePlaybookRepoForADO,
				webhookEventRepoForADO,
				authService,
				githubAppManager,
				azureDevOpsManager,
				vcsRegistry,
				registryPublishingHandler,
				configStorageClient,
				ansibleQueueForADO,
			)

			// OAuth2 callback (frontend redirects here after ADO authorization)
			adoWebhook.POST("/callback", adoHandler.CompleteAzureDevOpsInstallation)
			// Service Hook receiver (Azure DevOps sends push/PR events here)
			adoWebhook.POST("/webhook", adoHandler.HandleAzureDevOpsWebhook)
		}
	}

	// Registry Routes (Public - No Auth Required for Terraform CLI)
	// These endpoints are outside the authenticated v2 group
	// Note: moduleRepo, moduleVersionRepo, moduleDownloadRepo, registryStorage, and storageBucket
	// are already initialized above for the publishing routes
	moduleService := registry.NewModuleService(moduleRepo, moduleVersionRepo, moduleDownloadRepo, orgRepo, registryStorage, storageBucket)
	moduleHandler := handlers.NewRegistryModuleHandler(moduleService)

	// Initialize provider repositories and services
	providerRepo := repository.NewProviderRepository(db)
	providerVersionRepo := repository.NewProviderVersionRepository(db)
	providerPlatformRepo := repository.NewProviderPlatformRepository(db)
	providerDownloadRepo := repository.NewProviderDownloadRepository(db)
	providerService := registry.NewProviderService(providerRepo, providerVersionRepo, providerPlatformRepo, providerDownloadRepo, orgRepo, registryStorage, storageBucket)
	providerHandler := handlers.NewRegistryProviderHandler(providerService)

	// GPG Key Repository and Handler
	gpgKeyRepo := repository.NewGPGKeyRepository(db)
	gpgKeyHandler := handlers.NewGPGKeyHandler(gpgKeyRepo, orgRepo, authService)

	// Provider Publishing Handler
	providerPublishingHandler := handlers.NewRegistryProviderPublishingHandler(
		providerRepo,
		providerVersionRepo,
		providerPlatformRepo,
		orgRepo,
		gpgKeyRepo,
		authService,
		registryStorage,
		storageBucket,
	)

	// Module Registry (v1) - Public endpoints
	registryV1Modules := r.Group("/v1/modules")
	{
		registryV1Modules.GET("", moduleHandler.ListModules)
		registryV1Modules.GET("/search", moduleHandler.SearchModules)
		registryV1Modules.GET("/:namespace", moduleHandler.ListModules)
		registryV1Modules.GET("/:namespace/:name/:provider/versions", moduleHandler.GetModuleVersions)
		registryV1Modules.GET("/:namespace/:name/:provider", moduleHandler.GetModule)
		registryV1Modules.GET("/:namespace/:name/:provider/:version", moduleHandler.GetModuleVersion)
		registryV1Modules.GET("/:namespace/:name/:provider/:version/download", moduleHandler.DownloadModule)
		registryV1Modules.GET("/:namespace/:name/:provider/download", moduleHandler.DownloadModule)
	}

	// Provider Registry (v1) - Public endpoints
	registryV1Providers := r.Group("/v1/providers")
	{
		registryV1Providers.GET("", providerHandler.ListProviders)
		registryV1Providers.GET("/search", providerHandler.SearchProviders)
		registryV1Providers.GET("/:namespace", providerHandler.ListProviders)
		registryV1Providers.GET("/:namespace/:name/versions", providerHandler.GetProviderVersions)
		registryV1Providers.GET("/:namespace/:name", providerHandler.GetProvider)
		registryV1Providers.GET("/:namespace/:name/:version", providerHandler.GetProviderVersion)
		registryV1Providers.GET("/:namespace/:name/:version/download/:os/:arch", providerHandler.DownloadProvider)
		registryV1Providers.GET("/:namespace/:name/download/:os/:arch", providerHandler.DownloadProvider)
	}

	// Module Registry (v2) - Metrics endpoints
	registryV2Modules := r.Group("/v2/modules")
	{
		registryV2Modules.GET("/:namespace/:name/:provider/downloads/summary", moduleHandler.GetModuleDownloadsSummary)
	}

	// Provider Registry (v2) - Metrics endpoints
	registryV2Providers := r.Group("/v2/providers")
	{
		registryV2Providers.GET("/:namespace/:name/downloads/summary", providerHandler.GetProviderDownloadsSummary)
	}

	// Provider Publishing Routes (Authenticated)
	orgRegistryProviders := v2.Group("/organizations/:name/registry/providers")
	{
		orgRegistryProviders.POST("", providerPublishingHandler.CreateProvider)
		orgRegistryProviders.GET("", providerPublishingHandler.ListProviders)
		orgRegistryProviders.GET("/:provider_name", providerPublishingHandler.GetProvider)
		orgRegistryProviders.POST("/:provider_name/versions/:version/platforms", providerPublishingHandler.PublishProviderPlatform)
	}

	// GPG Key Management Routes (Authenticated)
	orgRegistryGPGKeys := v2.Group("/organizations/:name/registry/gpg-keys")
	{
		orgRegistryGPGKeys.POST("", gpgKeyHandler.CreateGPGKey)
		orgRegistryGPGKeys.GET("", gpgKeyHandler.ListGPGKeys)
		orgRegistryGPGKeys.DELETE("/:key_id", gpgKeyHandler.DeleteGPGKey)
	}

	// Activity/Audit Log Routes (activityService already created above)
	activityHandler := handlers.NewActivityHandlerV2(activityService, authService)

	activities := v2.Group("/activities")
	{
		activities.GET("", activityHandler.ListActivities)
		activities.GET("/recent", activityHandler.GetRecentActivities)
	}

	// ==========================================
	// Dashboard Routes
	// ==========================================
	// Initialize Ansible repositories for dashboard (needed for stats)
	ansibleInventoryRepo := repository.NewAnsibleInventoryRepository(db)
	ansiblePlaybookRepo := repository.NewAnsiblePlaybookRepository(db)
	ansibleJobTemplateRepo := repository.NewAnsibleJobTemplateRepository(db)
	ansibleJobRepo := repository.NewAnsibleJobRepository(db)
	ansibleCredentialRepo := repository.NewAnsibleCredentialRepository(db)

	// Dashboard handler
	dashboardHandler := handlers.NewDashboardHandler(
		orgRepo,
		projectRepo,
		workspaceRepo,
		runRepo,
		ansibleJobRepo,
		ansiblePlaybookRepo,
		authService,
	)

	dashboard := v2.Group("/dashboard")
	{
		dashboard.GET("/stats", dashboardHandler.GetStats)
	}

	// ==========================================
	// Ansible Routes Setup
	// ==========================================

	// Initialize Redis queue for Ansible jobs
	redisHost := os.Getenv("REDIS_HOST")
	if redisHost == "" {
		redisHost = "localhost"
	}
	redisPort := 6379
	if portStr := os.Getenv("REDIS_PORT"); portStr != "" {
		if p, err := parsePort(portStr); err == nil {
			redisPort = p
		}
	}
	redisPassword := os.Getenv("REDIS_PASSWORD")

	var ansibleRedisQueue *queue.RedisQueue
	ansibleRedisQueue, err = queue.NewRedisQueue(redisHost, redisPort, redisPassword, 0)
	if err != nil {
		logger.Warnf("Failed to initialize Redis queue for Ansible: %v (Ansible job queueing will be disabled)", err)
	}

	// Get encryption key for Ansible credentials
	ansibleEncryptionKey := os.Getenv("ANSIBLE_ENCRYPTION_KEY")
	if ansibleEncryptionKey == "" {
		ansibleEncryptionKey = os.Getenv("ENCRYPTION_KEY")
	}
	var encryptionKeyBytes []byte
	if ansibleEncryptionKey != "" {
		var decodeErr error
		encryptionKeyBytes, decodeErr = hex.DecodeString(ansibleEncryptionKey)
		if decodeErr != nil {
			logger.Warnf("Failed to decode Ansible encryption key: %v (using raw bytes)", decodeErr)
			encryptionKeyBytes = []byte(ansibleEncryptionKey)
		}
		// Ensure key is 32 bytes for AES-256
		if len(encryptionKeyBytes) < 32 {
			paddedKey := make([]byte, 32)
			copy(paddedKey, encryptionKeyBytes)
			encryptionKeyBytes = paddedKey
		} else if len(encryptionKeyBytes) > 32 {
			encryptionKeyBytes = encryptionKeyBytes[:32]
		}
	} else {
		logger.Warn("No encryption key configured for Ansible credentials (set ANSIBLE_ENCRYPTION_KEY or ENCRYPTION_KEY)")
		// Use a default key for development only - this should be overridden in production
		encryptionKeyBytes = make([]byte, 32)
	}

	// Setup Ansible routes
	SetupAnsibleRoutes(
		v2,
		db,
		ansibleInventoryRepo,
		ansiblePlaybookRepo,
		ansibleJobTemplateRepo,
		ansibleJobRepo,
		ansibleCredentialRepo,
		projectRepo,
		orgRepo,
		authService,
		ansibleRedisQueue,
		encryptionKeyBytes,
		vcsRegistry,
		vcsConnectionRepo,
	)

	// Initialize Ansible Workflow repository and routes
	ansibleWorkflowRepo := repository.NewAnsibleWorkflowRepository(db)
	SetupAnsibleWorkflowRoutes(
		v2,
		ansibleWorkflowRepo,
		orgRepo,
		projectRepo,
		authService,
	)

	// ==========================================
	// Runner Agent API (for self-hosted runners)
	// ==========================================
	// These endpoints are used by runner agents to register, heartbeat, and report job status.
	// They use API key authentication (same as the rest of v2).
	// Reference: SELF_HOSTED_RUNNERS_DESIGN.md
	// Initialize ansible config repo (not already created above)
	ansibleConfigRepo := repository.NewAnsibleConfigRepository(db)

	// Use the same encryption key as Ansible credentials so we can decrypt for self-hosted runner artifacts
	var runnerCryptoSvc *crypto.CryptoService
	if len(encryptionKeyBytes) > 0 {
		runnerCryptoSvc, _ = crypto.NewCryptoService(encryptionKeyBytes)
	}

	// Initialize inventory service for generating proper Ansible inventory content
	runnerInventoryService := ansible.NewInventoryService(ansibleInventoryRepo, orgRepo)

	runnerAgentHandler := handlers.NewRunnerAgentHandlerWithRepos(
		runnerRepo, runnerJobExecRepo, agentPoolRepo, nil,
		ansibleJobRepo, ansiblePlaybookRepo, ansibleInventoryRepo,
		ansibleCredentialRepo, ansibleConfigRepo, runnerInventoryService, vcsRegistry, runnerCryptoSvc,
		variableService, configStorageClient, db,
	)

	// Wire OIDC workload identity services for self-hosted runners
	// This allows the artifacts endpoint to generate OIDC tokens for runners
	{
		issuerURL := os.Getenv("OIDC_ISSUER_URL")
		if issuerURL == "" {
			issuerURL = os.Getenv("API_URL")
		}
		if issuerURL == "" {
			issuerURL = "http://localhost:8022"
		}
		oidcTokenService := oidc.NewTokenService(oidcSigningKey, issuerURL)
		runnerAgentHandler.SetOIDCServices(azureOIDCRepo, oidcTokenService)
	}

	runnerAgent := v2.Group("/runner")
	{
		runnerAgent.POST("/register", runnerAgentHandler.Register)
		runnerAgent.POST("/heartbeat", runnerAgentHandler.Heartbeat)
		runnerAgent.POST("/deregister", runnerAgentHandler.Deregister)
		runnerAgent.POST("/jobs/:id/start", runnerAgentHandler.JobStart)
		runnerAgent.POST("/jobs/:id/output", runnerAgentHandler.JobOutput)
		runnerAgent.POST("/jobs/:id/complete", runnerAgentHandler.JobComplete)
		runnerAgent.POST("/jobs/:id/state", runnerAgentHandler.UploadState)
		runnerAgent.GET("/jobs/:id/artifacts", runnerAgentHandler.GetJobArtifacts)
		runnerAgent.GET("/jobs/:id/status", runnerAgentHandler.GetJobStatus)
	}

	// Zitadel Actions V2 Webhooks (unauthenticated — Zitadel calls these directly)
	// These webhooks handle SSO group claim passthrough for automatic team assignment.
	// They use HMAC signature verification instead of JWT auth.
	// See: https://zitadel.com/docs/guides/integrate/actions/usage
	zitadelWebhookHandler := handlers.NewZitadelWebhookHandler()
	zitadelActions := r.Group("/api/v2/zitadel/actions")
	{
		// Response webhook for RetrieveIdentityProviderIntent:
		// Extracts group claims from external IdP and stores as user metadata
		zitadelActions.POST("/idp-sync", zitadelWebhookHandler.HandleIDPSync)

		// Function webhook for preaccesstoken:
		// Reads sso_groups metadata and appends as custom claim in access token
		zitadelActions.POST("/complement-token", zitadelWebhookHandler.HandleComplementToken)
	}
}

// parsePort parses a port string to int
func parsePort(portStr string) (int, error) {
	var port int
	_, err := fmt.Sscanf(portStr, "%d", &port)
	return port, err
}
