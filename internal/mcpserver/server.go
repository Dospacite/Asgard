package mcpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rousoftware/asgard/internal/auth"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/config"
	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/importer"
	"github.com/rousoftware/asgard/internal/networking"
	"github.com/rousoftware/asgard/internal/operations"
	"github.com/rousoftware/asgard/internal/projectsource"
	"github.com/rousoftware/asgard/internal/proxy"
	"github.com/rousoftware/asgard/internal/secrets"
	"github.com/rousoftware/asgard/internal/store"
)

type Dependencies struct {
	Config     config.Config
	Store      *store.Store
	Docker     *dockerx.Engine
	Networks   *networking.Manager
	Operations *operations.Manager
	Importer   *importer.Importer
	Proxy      *proxy.Generator
	Secrets    *secrets.Box
}
type Server struct {
	Dependencies
	MCP     *mcp.Server
	Handler http.Handler
}

type Empty struct{}
type IDInput struct {
	ID string `json:"id" jsonschema:"required,resource identifier"`
}
type ProjectInput struct {
	ProjectID string `json:"projectId" jsonschema:"required,project identifier or slug"`
}
type SourceUpdateInput struct {
	ProjectID string `json:"projectId" jsonschema:"required,project identifier or slug"`
	Path      string `json:"path" jsonschema:"required,path returned by project_source_get"`
	Content   string `json:"content" jsonschema:"required,replacement UTF-8 file content"`
	Revision  string `json:"revision" jsonschema:"required,current file revision returned by project_source_get"`
}
type ServiceInput struct {
	ServiceID string `json:"serviceId" jsonschema:"required,service identifier"`
}
type LogsInput struct {
	ServiceID string `json:"serviceId" jsonschema:"required,service identifier"`
	Tail      int    `json:"tail,omitempty" jsonschema:"number of recent lines,minimum=1,maximum=5000"`
	Since     string `json:"since,omitempty" jsonschema:"Docker-compatible timestamp or duration"`
}
type DeployInput struct {
	ProjectID      string `json:"projectId" jsonschema:"required,project identifier or slug"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}
type RollbackInput struct {
	ProjectID      string `json:"projectId" jsonschema:"required,project identifier or slug"`
	ReleaseID      string `json:"releaseId" jsonschema:"required,successful release identifier"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}
type GitImportInput struct {
	Name         string `json:"name" jsonschema:"required,project display name"`
	Slug         string `json:"slug" jsonschema:"required,DNS-safe project slug"`
	Description  string `json:"description,omitempty"`
	URL          string `json:"url" jsonschema:"required,HTTPS Git URL; also ssh:// or git@host:path when credentialId names an SSH credential"`
	Ref          string `json:"ref,omitempty"`
	CredentialID string `json:"credentialId,omitempty" jsonschema:"stored Git credential id or name; required for private repositories"`
	ComposePath  string `json:"composePath,omitempty"`
}
type GitCredentialCreateInput struct {
	Name     string `json:"name" jsonschema:"required,human-readable credential name"`
	Kind     string `json:"kind" jsonschema:"required,token for HTTPS or ssh for a deploy key"`
	Secret   string `json:"secret" jsonschema:"required,the access token or PEM-encoded private key; stored encrypted and never returned"`
	Username string `json:"username,omitempty" jsonschema:"HTTPS username; defaults to x-access-token"`
	Host     string `json:"host,omitempty" jsonschema:"optional bare hostname this credential belongs to, such as github.com"`
}
type ImageImportInput struct {
	Name        string `json:"name" jsonschema:"required,project display name"`
	Slug        string `json:"slug" jsonschema:"required,DNS-safe project slug"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image" jsonschema:"required,public OCI image reference or Docker Hub URL"`
	Port        int    `json:"port,omitempty"`
	Public      bool   `json:"public,omitempty"`
}
type ActionInput struct {
	ServiceID string `json:"serviceId" jsonschema:"required,service identifier"`
	Action    string `json:"action" jsonschema:"required,start stop or restart"`
}
type ConfigInput struct {
	ServiceID      string            `json:"serviceId" jsonschema:"required,service identifier"`
	ConfigRevision int               `json:"configRevision" jsonschema:"required,optimistic concurrency revision"`
	Role           string            `json:"role" jsonschema:"required,web worker or stateful"`
	Environment    map[string]string `json:"environment,omitempty"`
	Public         bool              `json:"public"`
	Port           int               `json:"port"`
	Hostname       string            `json:"hostname,omitempty"`
	HealthPath     string            `json:"healthPath"`
	CPULimit       float64           `json:"cpuLimit"`
	MemoryLimit    int64             `json:"memoryLimit"`
	PIDsLimit      int64             `json:"pidsLimit"`
	RestartPolicy  string            `json:"restartPolicy"`
}
type NetworkInput struct {
	NetworkID string `json:"networkId" jsonschema:"required,managed network identifier or slug"`
}
type NetworkCreateInput struct {
	Name        string `json:"name" jsonschema:"required,network display name"`
	Slug        string `json:"slug,omitempty" jsonschema:"DNS-safe network slug; generated from name when omitted"`
	Description string `json:"description,omitempty"`
	Internal    bool   `json:"internal,omitempty" jsonschema:"block outbound internet through this network while preserving member communication"`
}
type NetworkMemberInput struct {
	NetworkID string `json:"networkId" jsonschema:"required,managed network identifier or slug"`
	ServiceID string `json:"serviceId" jsonschema:"required,service identifier"`
	Alias     string `json:"alias,omitempty" jsonschema:"network-scoped DNS alias; defaults to project--service"`
}
type NetworkDetachInput struct {
	NetworkID string `json:"networkId" jsonschema:"required,managed network identifier or slug"`
	ServiceID string `json:"serviceId" jsonschema:"required,service identifier"`
}
type DeletePreviewInput struct {
	TargetType string `json:"targetType" jsonschema:"required,project container or network"`
	TargetID   string `json:"targetId" jsonschema:"required,exact target identifier"`
}
type DeleteConfirmInput struct {
	ConfirmationToken string `json:"confirmationToken" jsonschema:"required,short-lived token returned by deletion_preview"`
}

func New(deps Dependencies) *Server {
	s := &Server{Dependencies: deps}
	server := mcp.NewServer(&mcp.Implementation{Name: "asgard", Title: "Asgard", Description: "Operate the Asgard single-host application cloud.", Version: "0.2.0", WebsiteURL: deps.Config.PublicURL}, &mcp.ServerOptions{Instructions: "Inspect current state before mutation. Use idempotency keys for deployments. Shared networks provide explicit cross-project connectivity with network-scoped DNS aliases. Deletion always requires deletion_preview followed by deletion_confirm with its exact short-lived token."})
	s.MCP = server
	s.addTools()
	s.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 2 << 20, PropagateRequestCancellation: true})
	return s
}

func readTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Title: title, Description: description, Annotations: &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(false)}}
}
func writeTool(name, title, description string, idempotent, destructive, openWorld bool) *mcp.Tool {
	return &mcp.Tool{Name: name, Title: title, Description: description, Annotations: &mcp.ToolAnnotations{Title: title, ReadOnlyHint: false, IdempotentHint: idempotent, OpenWorldHint: boolPtr(openWorld), DestructiveHint: boolPtr(destructive)}}
}
func boolPtr(value bool) *bool { return &value }

func (s *Server) addTools() {
	mcp.AddTool(s.MCP, readTool("system_get", "Get system capacity", "Return control-plane, Docker host, disk, CPU, and memory capacity."), func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"host": s.Docker.Host(ctx), "publicUrl": s.Config.PublicURL, "domain": s.Config.Domain, "mcpUrl": s.Config.PublicURL + "/mcp", "timezone": s.Config.Timezone.String()}, nil
	})
	mcp.AddTool(s.MCP, readTool("compose_contract_get", "Get Compose contract", "Return the exact safe Docker Compose subset accepted by imports."), func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		return nil, composecfg.Contract, nil
	})
	mcp.AddTool(s.MCP, readTool("projects_list", "List projects", "List projects with services, runtime state, and latest metrics."), func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		items, err := s.Store.ListProjects(ctx)
		return nil, map[string]any{"items": items}, err
	})
	mcp.AddTool(s.MCP, readTool("project_get", "Get project", "Get one project by UUID or slug, including its services."), func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		item, err := s.Store.GetProject(ctx, in.ProjectID)
		return nil, item, err
	})
	mcp.AddTool(s.MCP, readTool("project_source_get", "Get project source", "Return the editable Compose file, Dockerfiles, .env file, validation state, and per-file revisions."), s.projectSource)
	mcp.AddTool(s.MCP, readTool("services_list", "List project services", "List the services belonging to a project."), func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		project, err := s.Store.GetProject(ctx, in.ProjectID)
		return nil, map[string]any{"items": project.Services}, err
	})
	mcp.AddTool(s.MCP, readTool("service_get", "Get service", "Get service configuration, runtime state, limits, and latest metrics."), func(ctx context.Context, _ *mcp.CallToolRequest, in ServiceInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		item, err := s.Store.GetService(ctx, in.ServiceID)
		return nil, item, err
	})
	mcp.AddTool(s.MCP, readTool("service_stats_get", "Get service metrics", "Return live and recent CPU, RAM, I/O, network, and PID metrics."), func(ctx context.Context, _ *mcp.CallToolRequest, in ServiceInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		item, err := s.Store.GetService(ctx, in.ServiceID)
		if err != nil {
			return nil, nil, err
		}
		var live any = item.Metrics
		if item.Runtime != nil && item.Runtime.State == "running" {
			if stats, statsErr := s.Docker.Stats(ctx, item.Runtime.DockerID); statsErr == nil {
				live = stats
			}
		}
		return nil, map[string]any{"current": live, "limits": map[string]any{"cpu": item.CPULimit, "memoryBytes": item.MemoryLimit, "pids": item.PIDsLimit}}, nil
	})
	mcp.AddTool(s.MCP, readTool("service_logs_get", "Get service logs", "Return bounded recent logs from the active service container."), func(ctx context.Context, _ *mcp.CallToolRequest, in LogsInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		item, err := s.Store.GetService(ctx, in.ServiceID)
		if err != nil {
			return nil, nil, err
		}
		if item.Runtime == nil {
			return nil, map[string]any{"available": false, "content": ""}, nil
		}
		content, err := s.Docker.Logs(ctx, item.Runtime.DockerID, in.Tail, in.Since)
		return nil, map[string]any{"available": true, "content": content, "containerId": item.Runtime.DockerID}, err
	})
	mcp.AddTool(s.MCP, readTool("deployments_list", "List deployments", "List recent deployment and rollback runs, optionally for one project."), func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		items, err := s.Store.ListDeployments(ctx, in.ProjectID, 100)
		return nil, map[string]any{"items": items}, err
	})
	mcp.AddTool(s.MCP, readTool("operation_get", "Get operation", "Get durable operation state and progress."), func(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		item, err := s.Store.GetOperation(ctx, in.ID)
		return nil, item, err
	})
	mcp.AddTool(s.MCP, readTool("operation_logs_get", "Get operation logs", "Get ordered build, deployment, backup, or restore logs."), func(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		items, err := s.Store.OperationLogs(ctx, in.ID, 5000)
		return nil, map[string]any{"items": items}, err
	})
	mcp.AddTool(s.MCP, readTool("unmanaged_containers_list", "List unmanaged containers", "List Docker containers that are not yet managed by Asgard."), func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		items, err := s.Docker.Containers(ctx, true)
		if err != nil {
			return nil, nil, err
		}
		out := items[:0]
		for _, item := range items {
			if !item.Managed {
				out = append(out, item)
			}
		}
		return nil, map[string]any{"items": out}, nil
	})
	mcp.AddTool(s.MCP, readTool("networks_list", "List shared networks", "List Asgard-managed shared networks, attached services, aliases, live addresses, and Docker state."), s.networksList)
	mcp.AddTool(s.MCP, readTool("network_topology_get", "Get network topology", "Return public-edge, project-private, and shared-network nodes with every service connection."), s.networkTopology)

	mcp.AddTool(s.MCP, writeTool("project_import_git", "Import Git project", "Clone an HTTPS or SSH repository, validate its safe Compose file, and create a project. Pass credentialId to reach a private repository.", false, false, true), s.importGit)
	mcp.AddTool(s.MCP, readTool("git_credentials_list", "List Git credentials", "List stored Git credentials by name, kind, and host. Secrets are never returned."), func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		if err := require(ctx, "asgard:read"); err != nil {
			return nil, nil, err
		}
		items, err := s.Store.ListGitCredentials(ctx)
		return nil, map[string]any{"items": items}, err
	})
	mcp.AddTool(s.MCP, writeTool("git_credential_create", "Store a Git credential", "Encrypt and store an access token or SSH deploy key so private repositories can be imported. The secret is write-only.", false, false, false), s.createGitCredential)
	mcp.AddTool(s.MCP, writeTool("git_credential_delete", "Delete a Git credential", "Remove a stored Git credential. Projects already imported with it keep their source.", true, true, false), s.deleteGitCredential)
	mcp.AddTool(s.MCP, writeTool("project_import_image", "Import public OCI image", "Create a project from a public OCI image or Docker Hub URL.", false, false, true), s.importImage)
	mcp.AddTool(s.MCP, writeTool("project_source_update", "Update project source file", "Revision-check, validate, and replace one editable Compose, Dockerfile, or .env file. Compose saves reconcile source-owned service fields while preserving runtime overrides.", true, false, false), s.updateProjectSource)
	mcp.AddTool(s.MCP, writeTool("deployment_create", "Deploy project", "Queue an idempotent, health-gated versioned deployment and return its operation.", true, false, true), s.deploy)
	mcp.AddTool(s.MCP, writeTool("deployment_rollback", "Roll back project", "Queue recreation of an earlier successful release and atomically switch traffic.", true, false, false), s.rollback)
	mcp.AddTool(s.MCP, writeTool("service_config_update", "Update service configuration", "Update limits, public route, environment, role, and restart behavior with revision protection.", true, false, false), s.updateConfig)
	mcp.AddTool(s.MCP, writeTool("container_action", "Act on service container", "Start, stop, or restart the active service container.", false, false, false), s.containerAction)
	mcp.AddTool(s.MCP, writeTool("network_create", "Create shared network", "Create an Asgard-managed bridge network for explicit service communication across projects.", false, false, false), s.networkCreate)
	mcp.AddTool(s.MCP, writeTool("network_service_attach", "Attach service to network", "Persist and immediately reconcile a service membership with a stable network-scoped DNS alias.", false, false, false), s.networkAttach)
	mcp.AddTool(s.MCP, writeTool("network_service_detach", "Detach service from network", "Disconnect a service from a shared network and remove its persisted membership.", true, false, false), s.networkDetach)
	mcp.AddTool(s.MCP, writeTool("network_reconcile", "Reconcile shared network", "Ensure the Docker network exists and reconnect every deployed member from persisted state.", true, false, false), s.networkReconcile)
	mcp.AddTool(s.MCP, writeTool("deletion_preview", "Preview exact deletion", "Resolve the exact destructive impact and return a five-minute confirmation token.", true, false, false), s.deletionPreview)
	mcp.AddTool(s.MCP, writeTool("deletion_confirm", "Confirm exact deletion", "Consume a preview token and delete only its exact bound project, container, or empty shared network. Named volumes are retained.", false, true, false), s.deletionConfirm)
}

func (s *Server) projectSource(ctx context.Context, _ *mcp.CallToolRequest, in ProjectInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:read"); err != nil {
		return nil, nil, err
	}
	project, err := s.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	workspace, err := projectsource.Load(project)
	return nil, workspace, err
}

func (s *Server) updateProjectSource(ctx context.Context, _ *mcp.CallToolRequest, in SourceUpdateInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	project, err := s.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	workspace, err := projectsource.Save(ctx, s.Store, project, s.Config.Domain, in.Path, in.Content, in.Revision)
	if err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "project.source.update", "project", project.ID, "Agent updated source file "+filepath.ToSlash(filepath.Clean(in.Path)))
	return nil, workspace, nil
}

func (s *Server) networksList(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:read"); err != nil {
		return nil, nil, err
	}
	items, err := s.Networks.List(ctx)
	return nil, map[string]any{"items": items}, err
}

func (s *Server) networkTopology(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:read"); err != nil {
		return nil, nil, err
	}
	item, err := s.Networks.Topology(ctx)
	return nil, item, err
}

func (s *Server) networkCreate(ctx context.Context, _ *mcp.CallToolRequest, in NetworkCreateInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 || len(in.Description) > 1000 {
		return nil, nil, errors.New("network name is required and limited to 100 characters; description is limited to 1,000 characters")
	}
	slug := composecfg.Slug(in.Slug)
	if slug == "" {
		slug = composecfg.Slug(name)
	}
	if !composecfg.ValidateSlug(slug) {
		return nil, nil, errors.New("network slug must be a valid DNS label")
	}
	item, err := s.Networks.Create(ctx, store.ManagedNetwork{ID: uuid.NewString(), Slug: slug, Name: name, DockerName: "asgard-shared-" + slug, Description: strings.TrimSpace(in.Description), Driver: "bridge", Internal: in.Internal})
	if err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "network.create", "network", item.ID, "Agent created shared network "+item.Name)
	view, err := s.Networks.Get(ctx, item.ID)
	return nil, view, err
}

func (s *Server) networkAttach(ctx context.Context, _ *mcp.CallToolRequest, in NetworkMemberInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.NetworkID) == "" || strings.TrimSpace(in.ServiceID) == "" {
		return nil, nil, errors.New("networkId and serviceId are required")
	}
	if in.Alias != "" && networking.NormalizeAlias(in.Alias) == "" {
		return nil, nil, errors.New("DNS alias must contain an ASCII letter or number")
	}
	item, err := s.Networks.Attach(ctx, in.NetworkID, in.ServiceID, in.Alias)
	if err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "network.attach", "network", item.ID, "Agent attached service "+in.ServiceID+" to shared network")
	view, err := s.Networks.Get(ctx, item.ID)
	return nil, view, err
}

func (s *Server) networkDetach(ctx context.Context, _ *mcp.CallToolRequest, in NetworkDetachInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	item, err := s.Networks.Detach(ctx, in.NetworkID, in.ServiceID)
	if err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "network.detach", "network", item.ID, "Agent detached service "+in.ServiceID+" from shared network")
	view, err := s.Networks.Get(ctx, item.ID)
	return nil, view, err
}

func (s *Server) networkReconcile(ctx context.Context, _ *mcp.CallToolRequest, in NetworkInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	item, err := s.Networks.Reconcile(ctx, in.NetworkID)
	if err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "network.reconcile", "network", item.ID, "Agent reconciled shared network endpoints")
	view, err := s.Networks.Get(ctx, item.ID)
	return nil, view, err
}

func (s *Server) importGit(ctx context.Context, _ *mcp.CallToolRequest, in GitImportInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:deploy"); err != nil {
		return nil, nil, err
	}
	project, result, err := s.Importer.FromGit(ctx, importer.Request{Name: in.Name, Slug: in.Slug, Description: in.Description, URL: in.URL, Ref: in.Ref, CredentialID: in.CredentialID, ComposePath: in.ComposePath})
	if err == nil {
		visibility := "public"
		if in.CredentialID != "" {
			visibility = "private"
		}
		audit(ctx, s.Store, "project.import.git", "project", project.ID, "Agent imported "+visibility+" Git project")
	}
	return nil, map[string]any{"project": project, "validation": result}, err
}
func (s *Server) createGitCredential(ctx context.Context, _ *mcp.CallToolRequest, in GitCredentialCreateInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	if s.Secrets == nil {
		return nil, nil, errors.New("credential storage is unavailable")
	}
	item, secret, err := store.NormalizeGitCredential(in.Name, in.Kind, in.Username, in.Host, in.Secret)
	if err != nil {
		return nil, nil, err
	}
	ciphertext, nonce, err := s.Secrets.Seal(secret)
	if err != nil {
		return nil, nil, err
	}
	created, err := s.Store.CreateGitCredential(ctx, item, ciphertext, nonce)
	if err == nil {
		audit(ctx, s.Store, "git_credential.create", "git_credential", created.ID, "Agent stored "+created.Kind+" credential "+created.Name)
	}
	return nil, created, err
}

func (s *Server) deleteGitCredential(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	item, err := s.Store.GetGitCredential(ctx, in.ID)
	if err != nil {
		return nil, nil, errors.New("git credential not found")
	}
	projects, err := s.Store.ProjectsUsingGitCredential(ctx, item.ID)
	if err != nil {
		return nil, nil, err
	}
	if err = s.Store.DeleteGitCredential(ctx, item.ID); err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "git_credential.delete", "git_credential", item.ID, "Agent deleted credential "+item.Name)
	return nil, map[string]any{"deleted": item.ID, "name": item.Name, "unlinkedProjects": projects}, nil
}

func (s *Server) importImage(ctx context.Context, _ *mcp.CallToolRequest, in ImageImportInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:deploy"); err != nil {
		return nil, nil, err
	}
	project, result, err := s.Importer.FromImage(ctx, importer.Request{Name: in.Name, Slug: in.Slug, Description: in.Description, Image: in.Image, Port: in.Port, Public: in.Public})
	if err == nil {
		audit(ctx, s.Store, "project.import.image", "project", project.ID, "Agent imported public OCI image")
	}
	return nil, map[string]any{"project": project, "validation": result}, err
}
func (s *Server) deploy(ctx context.Context, _ *mcp.CallToolRequest, in DeployInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:deploy"); err != nil {
		return nil, nil, err
	}
	project, err := s.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	actor, _ := auth.IdentityFrom(ctx)
	op := store.Operation{ID: uuid.NewString(), Kind: "deployment.create", TargetType: "project", TargetID: project.ID, Summary: "Deploy " + project.Name, RequestedBy: actor.UserID, Payload: jsonPayload(map[string]any{"trigger": "agent"})}
	op, err = s.Store.CreateOperation(ctx, op, in.IdempotencyKey)
	if err == nil {
		s.Operations.Enqueue(op.ID)
		audit(ctx, s.Store, "deployment.create", "project", project.ID, "Agent queued deployment")
	}
	return nil, op, err
}
func (s *Server) rollback(ctx context.Context, _ *mcp.CallToolRequest, in RollbackInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:deploy"); err != nil {
		return nil, nil, err
	}
	project, err := s.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	release, err := s.Store.GetRelease(ctx, in.ReleaseID)
	if err != nil || release.ProjectID != project.ID || release.Status != "succeeded" {
		return nil, nil, errors.New("release must be a successful release from this project")
	}
	actor, _ := auth.IdentityFrom(ctx)
	op := store.Operation{ID: uuid.NewString(), Kind: "deployment.rollback", TargetType: "project", TargetID: project.ID, Summary: "Roll back " + project.Name, RequestedBy: actor.UserID, Payload: jsonPayload(map[string]any{"trigger": "rollback", "rollbackReleaseId": release.ID})}
	op, err = s.Store.CreateOperation(ctx, op, in.IdempotencyKey)
	if err == nil {
		s.Operations.Enqueue(op.ID)
		audit(ctx, s.Store, "deployment.rollback", "project", project.ID, "Agent queued rollback")
	}
	return nil, op, err
}
func (s *Server) updateConfig(ctx context.Context, _ *mcp.CallToolRequest, in ConfigInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:configure"); err != nil {
		return nil, nil, err
	}
	svc, err := s.Store.GetService(ctx, in.ServiceID)
	if err != nil {
		return nil, nil, err
	}
	if in.Role != "web" && in.Role != "worker" && in.Role != "stateful" {
		return nil, nil, errors.New("role must be web, worker, or stateful")
	}
	if in.Public && (in.Port < 1 || in.Hostname == "" || !(in.Hostname == s.Config.Domain || strings.HasSuffix(in.Hostname, "."+s.Config.Domain)) || in.Hostname == s.Config.Domain) {
		return nil, nil, errors.New("public service needs a valid port and non-control-plane hostname inside the Asgard domain")
	}
	if err := composecfg.ValidateEnvironment(in.Environment); err != nil {
		return nil, nil, err
	}
	svc.Role = in.Role
	svc.Environment = in.Environment
	svc.Public = in.Public
	svc.Port = in.Port
	svc.Hostname = strings.ToLower(in.Hostname)
	svc.HealthPath = in.HealthPath
	svc.CPULimit = in.CPULimit
	svc.MemoryLimit = in.MemoryLimit
	svc.PIDsLimit = in.PIDsLimit
	svc.RestartPolicy = in.RestartPolicy
	if err = s.Store.UpdateService(ctx, svc, in.ConfigRevision); err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "service.update", "service", svc.ID, "Agent updated service configuration")
	updated, err := s.Store.GetService(ctx, svc.ID)
	return nil, updated, err
}
func (s *Server) containerAction(ctx context.Context, _ *mcp.CallToolRequest, in ActionInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:operate"); err != nil {
		return nil, nil, err
	}
	svc, err := s.Store.GetService(ctx, in.ServiceID)
	if err != nil {
		return nil, nil, err
	}
	if svc.Runtime == nil {
		return nil, nil, errors.New("service has no active container")
	}
	if err = s.Docker.Action(ctx, svc.Runtime.DockerID, in.Action); err != nil {
		return nil, nil, err
	}
	audit(ctx, s.Store, "service."+in.Action, "service", svc.ID, "Agent performed container action")
	return nil, map[string]any{"serviceId": svc.ID, "containerId": svc.Runtime.DockerID, "action": in.Action}, nil
}

func (s *Server) deletionPreview(ctx context.Context, _ *mcp.CallToolRequest, in DeletePreviewInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:delete"); err != nil {
		return nil, nil, err
	}
	preview := map[string]any{"targetType": in.TargetType, "targetId": in.TargetID, "namedVolumesRetained": true}
	switch in.TargetType {
	case "project":
		project, err := s.Store.GetProject(ctx, in.TargetID)
		if err != nil {
			return nil, nil, err
		}
		preview["name"] = project.Name
		preview["services"] = len(project.Services)
		preview["sourceRemoved"] = true
	case "container":
		item, err := s.Docker.Container(ctx, in.TargetID)
		if err != nil {
			return nil, nil, err
		}
		preview["name"] = item.Name
		preview["managed"] = item.Managed
	case "network":
		item, err := s.Networks.Get(ctx, in.TargetID)
		if err != nil {
			return nil, nil, err
		}
		if len(item.Members) > 0 {
			return nil, nil, errors.New("disconnect every service before deleting this network")
		}
		preview["name"] = item.Name
		preview["slug"] = item.Slug
		preview["members"] = 0
		preview["internal"] = item.Internal
		preview["dockerNetworkRemoved"] = true
	default:
		return nil, nil, errors.New("targetType must be project, container, or network")
	}
	token := randomToken(32)
	actor, _ := auth.IdentityFrom(ctx)
	id := uuid.NewString()
	bytes, _ := json.Marshal(preview)
	expires := time.Now().UTC().Add(5 * time.Minute)
	_, err := s.Store.DB.ExecContext(ctx, `INSERT INTO deletion_intents(id,token_hash,target_type,target_id,actor_id,preview_json,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, auth.HashToken(token), in.TargetType, in.TargetID, actor.UserID, string(bytes), expires.Format(time.RFC3339Nano), store.Now())
	return nil, map[string]any{"confirmationToken": token, "expiresAt": expires, "preview": preview}, err
}
func (s *Server) deletionConfirm(ctx context.Context, _ *mcp.CallToolRequest, in DeleteConfirmInput) (*mcp.CallToolResult, any, error) {
	if err := require(ctx, "asgard:delete"); err != nil {
		return nil, nil, err
	}
	actor, _ := auth.IdentityFrom(ctx)
	var id, targetType, targetID, expires string
	var used sql.NullString
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id,target_type,target_id,expires_at,used_at FROM deletion_intents WHERE token_hash=? AND actor_id=?`, auth.HashToken(in.ConfirmationToken), actor.UserID).Scan(&id, &targetType, &targetID, &expires, &used)
	if err != nil {
		return nil, nil, errors.New("confirmation token is invalid")
	}
	expiry, _ := time.Parse(time.RFC3339Nano, expires)
	if used.Valid || time.Now().After(expiry) {
		return nil, nil, errors.New("confirmation token expired or was already used")
	}
	result, err := s.Store.DB.ExecContext(ctx, `UPDATE deletion_intents SET used_at=? WHERE id=? AND used_at IS NULL`, store.Now(), id)
	if err != nil {
		return nil, nil, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return nil, nil, errors.New("confirmation token was already used")
	}
	switch targetType {
	case "project":
		project, err := s.Store.GetProject(ctx, targetID)
		if err != nil {
			return nil, nil, err
		}
		runtimes, _ := s.Store.ActiveRuntimes(ctx, project.ID)
		for _, runtime := range runtimes {
			if err := s.Docker.Remove(ctx, runtime.DockerID, false); err != nil {
				return nil, nil, err
			}
		}
		if err := s.Docker.RemoveProjectNetwork(ctx, "asgard-project-"+project.Slug, project.ID); err != nil {
			return nil, nil, err
		}
		if _, err := s.Store.DB.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, project.ID); err != nil {
			return nil, nil, err
		}
		if err := os.RemoveAll(project.SourcePath); err != nil {
			return nil, nil, err
		}
		_ = s.Proxy.Write(ctx)
	case "container":
		if err := s.Docker.Remove(ctx, targetID, false); err != nil {
			return nil, nil, err
		}
	case "network":
		if err := s.Networks.Delete(ctx, targetID); err != nil {
			return nil, nil, err
		}
	}
	audit(ctx, s.Store, targetType+".delete", targetType, targetID, "Agent confirmed exact deletion")
	return nil, map[string]any{"deleted": true, "targetType": targetType, "targetId": targetID, "namedVolumesRetained": true}, nil
}

func require(ctx context.Context, scope string) error {
	identity, ok := auth.IdentityFrom(ctx)
	if !ok || identity.ActorType != "agent" {
		return errors.New("OAuth agent identity is required")
	}
	for _, value := range strings.Fields(identity.Scope) {
		if value == scope {
			return nil
		}
	}
	return fmt.Errorf("OAuth scope %s is required", scope)
}
func audit(ctx context.Context, database *store.Store, action, targetType, targetID, summary string) {
	identity, _ := auth.IdentityFrom(ctx)
	_ = database.Audit(context.WithoutCancel(ctx), "agent", identity.UserID, action, targetType, targetID, summary, "mcp", "")
}
func jsonPayload(value any) json.RawMessage { bytes, _ := json.Marshal(value); return bytes }
func randomToken(size int) string {
	bytes := make([]byte, size)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}
