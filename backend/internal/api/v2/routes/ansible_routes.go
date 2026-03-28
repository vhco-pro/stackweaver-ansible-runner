// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package routes

import (
	"os"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/api/v2/handlers"
	ansibleHandlers "github.com/iac-platform/backend/internal/api/v2/handlers/ansible"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/variable"
	vcs "github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/pkg/crypto"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

// SetupAnsibleWorkflowRoutes sets up workflow-specific routes (called after other repos are initialized)
func SetupAnsibleWorkflowRoutes(
	v2 *gin.RouterGroup,
	workflowRepo *repository.AnsibleWorkflowRepository,
	orgRepo *repository.OrganizationRepository,
	projectRepo *repository.ProjectRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) {
	// Initialize Workflow Handler
	workflowHandler := ansibleHandlers.NewWorkflowHandler(workflowRepo, orgRepo, projectRepo, authService, rbacService)

	// ==========================================
	// Ansible Workflow Template Routes
	// ==========================================

	// Organization-scoped workflow endpoints
	// GET/POST /api/v2/organizations/:name/ansible/workflows
	orgWorkflows := v2.Group("/organizations/:name/ansible/workflows")
	{
		orgWorkflows.GET("", workflowHandler.List)
		orgWorkflows.POST("", workflowHandler.Create)
	}

	// Workflow by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/workflows/:id
	workflows := v2.Group("/ansible/workflows")
	{
		workflows.GET("/:id", workflowHandler.Get)
		workflows.PATCH("/:id", workflowHandler.Update)
		workflows.DELETE("/:id", workflowHandler.Delete)
		// Nodes management
		workflows.GET("/:id/nodes", workflowHandler.ListNodes)
		workflows.POST("/:id/nodes", workflowHandler.CreateNode)
		// Edges management
		workflows.GET("/:id/edges", workflowHandler.ListEdges)
		workflows.POST("/:id/edges", workflowHandler.CreateEdge)
	}

	// Workflow Node by ID endpoints
	// PATCH/DELETE /api/v2/ansible/workflow-nodes/:id
	workflowNodes := v2.Group("/ansible/workflow-nodes")
	{
		workflowNodes.PATCH("/:id", workflowHandler.UpdateNode)
		workflowNodes.DELETE("/:id", workflowHandler.DeleteNode)
	}

	// Workflow Edge by ID endpoints
	// DELETE /api/v2/ansible/workflow-edges/:id
	workflowEdges := v2.Group("/ansible/workflow-edges")
	{
		workflowEdges.DELETE("/:id", workflowHandler.DeleteEdge)
	}
}

// SetupAnsibleRoutes sets up the Ansible-related API routes
func SetupAnsibleRoutes(
	v2 *gin.RouterGroup,
	db *gorm.DB,
	inventoryRepo *repository.AnsibleInventoryRepository,
	playbookRepo *repository.AnsiblePlaybookRepository,
	templateRepo *repository.AnsibleJobTemplateRepository,
	jobRepo *repository.AnsibleJobRepository,
	credentialRepo *repository.AnsibleCredentialRepository,
	projectRepo *repository.ProjectRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
	redisQueue queue.Queue,
	encryptionKey []byte,
	vcsRegistry *vcs.ProviderRegistry,
	vcsConnectionRepo *repository.VCSConnectionRepository,
) {
	// Initialize Ansible Services
	credentialService := ansible.NewCredentialService(credentialRepo, encryptionKey)
	inventoryService := ansible.NewInventoryService(inventoryRepo, orgRepo)

	// Initialize variable service for Ansible (with variable sets, workspace, and template variable support)
	// Create repositories needed for variable service
	workspaceRepo := repository.NewWorkspaceRepository(db)
	variableRepo := repository.NewVariableRepository(db)
	variableSetRepo := repository.NewVariableSetRepository(db)
	templateVariableRepo := repository.NewAnsibleJobTemplateVariableRepository(db)
	variableService := variable.NewServiceWithTemplateVariables(variableRepo, variableSetRepo, workspaceRepo, templateVariableRepo, encryptionKey)

	// Create job service with variable set support
	jobService := ansible.NewJobServiceWithVariables(jobRepo, playbookRepo, inventoryRepo, templateRepo, projectRepo, variableService, redisQueue)

	// Initialize new repositories for inventory sources and schedules
	inventorySourceRepo := repository.NewAnsibleInventorySourceRepository(db)
	scheduleRepo := repository.NewAnsibleScheduleRepository(db)

	// Initialize crypto service for secure credential handling
	cryptoService, err := crypto.NewCryptoService(encryptionKey)
	if err != nil {
		logger.Warnf("Failed to initialize crypto service: %v (credential decryption will fail)", err)
	}

	// Initialize Inventory Source Service
	inventorySourceService := ansible.NewInventorySourceService(inventorySourceRepo, inventoryRepo, credentialRepo, cryptoService)

	// Wire OIDC workload identity support for Azure inventory sync (dynamic sources)
	azureOIDCRepo := repository.NewAzureOIDCConfigurationRepository(db)
	oidcSigningKey, oidcErr := oidc.NewSigningKey()
	if oidcErr != nil {
		logger.Warnf("Failed to initialize OIDC signing key for inventory source sync: %v (OIDC auth will be disabled)", oidcErr)
	} else {
		issuerURL := os.Getenv("OIDC_ISSUER_URL")
		if issuerURL == "" {
			issuerURL = os.Getenv("API_URL")
		}
		if issuerURL == "" {
			issuerURL = "http://localhost:8022"
		}
		oidcTokenService := oidc.NewTokenService(oidcSigningKey, issuerURL)
		inventorySourceService.SetOIDCServices(azureOIDCRepo, oidcTokenService)
		logger.Info("OIDC workload identity enabled for inventory source sync")
	}

	// Initialize Scheduler Service
	schedulerService := ansible.NewSchedulerService(
		scheduleRepo,
		jobRepo,
		templateRepo,
		playbookRepo,
		inventorySourceService,
		jobService,
	)

	// Initialize Variable Set Handler for job template variable sets (reuse from terraform routes)
	variableSetRepoForAnsible := repository.NewVariableSetRepository(db)
	variableSetVariableRepoForAnsible := repository.NewVariableSetVariableRepository(db)
	variableSetHandlerForAnsible := handlers.NewVariableSetHandlerV2(variableSetRepoForAnsible, variableSetVariableRepoForAnsible, orgRepo, projectRepo, nil, templateRepo, authService)

	// Initialize Ansible Handlers
	inventoryHandler := ansibleHandlers.NewInventoryHandler(inventoryService, inventoryRepo, orgRepo, projectRepo, authService, rbacService, redisQueue, vcsRegistry, vcsConnectionRepo)
	hostHandler := ansibleHandlers.NewHostHandler(inventoryService, inventoryRepo, authService)
	groupHandler := ansibleHandlers.NewGroupHandler(inventoryService, inventoryRepo, authService)
	credentialHandler := ansibleHandlers.NewCredentialHandler(credentialService, orgRepo, projectRepo, authService, rbacService)
	playbookHandler := ansibleHandlers.NewPlaybookHandler(playbookRepo, templateRepo, jobRepo, scheduleRepo, projectRepo, orgRepo, authService, rbacService, redisQueue, vcsRegistry, vcsConnectionRepo)
	jobHandler := ansibleHandlers.NewJobHandler(jobService, projectRepo, orgRepo, templateRepo, authService, rbacService)

	// Initialize new handlers
	inventorySourceHandler := ansibleHandlers.NewInventorySourceHandler(inventorySourceService, redisQueue)
	scheduleHandler := ansibleHandlers.NewScheduleHandler(schedulerService, orgRepo, authService, rbacService)
	collectionsHandler := ansibleHandlers.NewCollectionsHandler()

	// Initialize job template variable handler
	// Note: Template variables use project-level permissions (similar to other Ansible resources)
	templateVariableHandler := ansibleHandlers.NewJobTemplateVariableHandlerV2(templateVariableRepo, templateRepo, authService, nil, variableService)
	templateVariableHandler.SetRepositories(orgRepo, projectRepo)

	// ==========================================
	// Ansible Inventory Routes
	// ==========================================

	// Organization-scoped inventory endpoints
	// GET/POST /api/v2/organizations/:name/ansible/inventories
	orgInventories := v2.Group("/organizations/:name/ansible/inventories")
	{
		orgInventories.GET("", inventoryHandler.List)
		orgInventories.POST("", inventoryHandler.Create)
	}

	// Inventory by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/inventories/:id
	inventories := v2.Group("/ansible/inventories")
	{
		inventories.GET("/:id", inventoryHandler.Get)
		inventories.PATCH("/:id", inventoryHandler.Update)
		inventories.DELETE("/:id", inventoryHandler.Delete)

		// Sync action
		inventories.POST("/:id/actions/sync", inventoryHandler.SyncInventory)

		// Inventory format exports
		inventories.GET("/:id/ini", inventoryHandler.GetInventoryINI)
		inventories.GET("/:id/json", inventoryHandler.GetInventoryJSON)

		// Inventory Hosts
		// GET/POST /api/v2/ansible/inventories/:id/hosts
		inventories.GET("/:id/hosts", hostHandler.List)
		inventories.POST("/:id/hosts", hostHandler.Create)

		// Inventory Groups
		// GET/POST /api/v2/ansible/inventories/:id/groups
		inventories.GET("/:id/groups", groupHandler.List)
		inventories.POST("/:id/groups", groupHandler.Create)
	}

	// Host by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/hosts/:id
	hosts := v2.Group("/ansible/hosts")
	{
		hosts.GET("/:id", hostHandler.Get)
		hosts.PATCH("/:id", hostHandler.Update)
		hosts.DELETE("/:id", hostHandler.Delete)
	}

	// Group by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/groups/:id
	groups := v2.Group("/ansible/groups")
	{
		groups.GET("/:id", groupHandler.Get)
		groups.PATCH("/:id", groupHandler.Update)
		groups.DELETE("/:id", groupHandler.Delete)
	}

	// ==========================================
	// Ansible Credential Routes
	// ==========================================

	// Organization-scoped credential endpoints
	// GET/POST /api/v2/organizations/:name/ansible/credentials
	orgCredentials := v2.Group("/organizations/:name/ansible/credentials")
	{
		orgCredentials.GET("", credentialHandler.List)
		orgCredentials.POST("", credentialHandler.Create)
	}

	// Credential by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/credentials/:id
	credentials := v2.Group("/ansible/credentials")
	{
		credentials.GET("/:id", credentialHandler.Get)
		credentials.PATCH("/:id", credentialHandler.Update)
		credentials.DELETE("/:id", credentialHandler.Delete)
	}

	// ==========================================
	// Ansible Playbook Routes
	// ==========================================

	// Organization-scoped playbook endpoints (TFE-compatible pattern)
	// GET/POST /api/v2/organizations/:name/ansible/playbooks
	orgPlaybooks := v2.Group("/organizations/:name/ansible/playbooks")
	{
		orgPlaybooks.GET("", playbookHandler.ListPlaybooksByOrganization)
		orgPlaybooks.POST("", playbookHandler.CreatePlaybookByOrganization)
	}

	// Project-scoped playbook endpoints (for backward compatibility and querying by project)
	// GET /api/v2/projects/:id/ansible/playbooks
	projectPlaybooks := v2.Group("/projects/:id/ansible/playbooks")
	{
		projectPlaybooks.GET("", playbookHandler.ListPlaybooks)
	}

	// Playbook by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/playbooks/:id
	playbooks := v2.Group("/ansible/playbooks")
	{
		playbooks.GET("/:id", playbookHandler.GetPlaybook)
		playbooks.PATCH("/:id", playbookHandler.UpdatePlaybook)
		playbooks.DELETE("/:id", playbookHandler.DeletePlaybook)
		// Sync action
		playbooks.POST("/:id/actions/sync", playbookHandler.SyncPlaybook)
	}

	// ==========================================
	// Ansible Job Template Routes
	// ==========================================

	// Organization-scoped job template endpoints (TFE-compatible pattern)
	// GET/POST /api/v2/organizations/:name/ansible/job-templates
	orgJobTemplates := v2.Group("/organizations/:name/ansible/job-templates")
	{
		orgJobTemplates.GET("", playbookHandler.ListTemplatesByOrganization)
		orgJobTemplates.POST("", playbookHandler.CreateTemplateByOrganization)
	}

	// Project-scoped job template endpoints (for backward compatibility and querying by project)
	// GET /api/v2/projects/:id/ansible/job-templates
	projectJobTemplates := v2.Group("/projects/:id/ansible/job-templates")
	{
		projectJobTemplates.GET("", playbookHandler.ListTemplates)
	}

	// Job Template by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/job-templates/:id
	jobTemplates := v2.Group("/ansible/job-templates")
	{
		jobTemplates.GET("/:id", playbookHandler.GetTemplate)
		jobTemplates.PATCH("/:id", playbookHandler.UpdateTemplate)
		jobTemplates.DELETE("/:id", playbookHandler.DeleteTemplate)
		// Launch from template
		jobTemplates.POST("/:id/launch", jobHandler.LaunchFromTemplate)
		// Variable sets that apply to this job template (inherited from project)
		jobTemplates.GET("/:id/variable-sets", variableSetHandlerForAnsible.ListVariableSetsByJobTemplate)
		// Template variables (individual variables for this job template)
		jobTemplates.GET("/:id/vars", templateVariableHandler.ListByJobTemplate)
		jobTemplates.POST("/:id/vars", templateVariableHandler.Create)
		jobTemplates.PATCH("/:id/vars/:variable_id", templateVariableHandler.Update)
		jobTemplates.DELETE("/:id/vars/:variable_id", templateVariableHandler.Delete)
	}

	// ==========================================
	// Ansible Job Routes
	// ==========================================

	// Organization-scoped job endpoints (TFE-compatible pattern)
	// GET/POST /api/v2/organizations/:name/ansible/jobs
	orgJobs := v2.Group("/organizations/:name/ansible/jobs")
	{
		orgJobs.GET("", jobHandler.ListByOrganization)
		orgJobs.POST("", jobHandler.LaunchByOrganization)
		orgJobs.GET("/queue", jobHandler.GetQueue)
	}

	// Project-scoped job endpoints (for backward compatibility and querying by project)
	// GET /api/v2/projects/:id/ansible/jobs
	projectJobs := v2.Group("/projects/:id/ansible/jobs")
	{
		projectJobs.GET("", jobHandler.ListByProject)
	}

	// Job by ID endpoints
	// GET /api/v2/ansible/jobs/:id
	jobs := v2.Group("/ansible/jobs")
	{
		jobs.GET("/:id", jobHandler.Get)
		jobs.DELETE("/:id", jobHandler.Delete)
		// Job actions
		jobs.POST("/:id/actions/cancel", jobHandler.Cancel)
		jobs.POST("/:id/actions/relaunch", jobHandler.Relaunch)
		// Job events and output
		jobs.GET("/:id/events", jobHandler.GetEvents)
		jobs.GET("/:id/output", jobHandler.GetOutput)
	}

	// ==========================================
	// Ansible Inventory Source Routes (Dynamic Inventories)
	// ==========================================

	// Inventory-scoped inventory source endpoints
	// GET/POST /api/v2/ansible/inventories/:id/sources
	inventorySources := v2.Group("/ansible/inventories/:id/sources")
	{
		inventorySources.GET("", inventorySourceHandler.List)
		inventorySources.POST("", inventorySourceHandler.Create)
	}

	// Inventory Source by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/inventory-sources/:id
	inventorySourcesByID := v2.Group("/ansible/inventory-sources")
	{
		inventorySourcesByID.GET("/:source_id", inventorySourceHandler.Get)
		inventorySourcesByID.PATCH("/:source_id", inventorySourceHandler.Update)
		inventorySourcesByID.DELETE("/:source_id", inventorySourceHandler.Delete)
		// Sync action
		inventorySourcesByID.POST("/:source_id/actions/sync", inventorySourceHandler.Sync)
	}

	// ==========================================
	// Ansible Schedule Routes
	// ==========================================

	// Organization-scoped schedule endpoints
	// GET/POST /api/v2/organizations/:name/ansible/schedules
	orgSchedules := v2.Group("/organizations/:name/ansible/schedules")
	{
		orgSchedules.GET("", scheduleHandler.ListByOrganization)
		orgSchedules.POST("", scheduleHandler.Create)
	}

	// Schedule by ID endpoints
	// GET/PATCH/DELETE /api/v2/ansible/schedules/:id
	schedules := v2.Group("/ansible/schedules")
	{
		schedules.GET("/:schedule_id", scheduleHandler.Get)
		schedules.PATCH("/:schedule_id", scheduleHandler.Update)
		schedules.DELETE("/:schedule_id", scheduleHandler.Delete)
		// Schedule actions
		schedules.POST("/:schedule_id/actions/enable", scheduleHandler.Enable)
		schedules.POST("/:schedule_id/actions/disable", scheduleHandler.Disable)
		schedules.POST("/:schedule_id/actions/run-now", scheduleHandler.RunNow)
	}

	// ==========================================
	// Ansible Galaxy Collections Routes
	// ==========================================

	// Collections endpoints
	// GET /api/v2/ansible/collections/pre-installed - List pre-installed collections
	// GET /api/v2/ansible/collections/search - Search Galaxy Hub
	collections := v2.Group("/ansible/collections")
	{
		collections.GET("/pre-installed", collectionsHandler.ListPreInstalledCollections)
		collections.GET("/search", collectionsHandler.SearchGalaxyCollections)
	}

	// Per-job collections (requirements.yml installs)
	// GET /api/v2/ansible/jobs/:id/collections
	jobs.GET("/:id/collections", collectionsHandler.ListJobCollections)
}
