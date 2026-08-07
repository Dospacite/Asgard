package networking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/store"
)

type Manager struct {
	Store       *store.Store
	Docker      *dockerx.Engine
	EdgeNetwork string
}

type NetworkView struct {
	store.ManagedNetwork
	Status       string                  `json:"status"`
	Runtime      *dockerx.NetworkDetails `json:"runtime,omitempty"`
	RuntimeError string                  `json:"runtimeError,omitempty"`
}

type TopologyProject struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	ServiceCount int    `json:"serviceCount"`
}

type TopologyService struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	State       string `json:"state"`
	DockerName  string `json:"dockerName,omitempty"`
	Public      bool   `json:"public"`
	Hostname    string `json:"hostname,omitempty"`
}

type TopologyNetwork struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	DockerName  string   `json:"dockerName"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Driver      string   `json:"driver"`
	Internal    bool     `json:"internal"`
	MemberCount int      `json:"memberCount"`
	Subnets     []string `json:"subnets"`
}

type TopologyConnection struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	NetworkID   string `json:"networkId"`
	ProjectID   string `json:"projectId"`
	ServiceID   string `json:"serviceId"`
	Alias       string `json:"alias"`
	Status      string `json:"status"`
	IPv4Address string `json:"ipv4Address,omitempty"`
}

type Topology struct {
	Projects    []TopologyProject    `json:"projects"`
	Services    []TopologyService    `json:"services"`
	Networks    []TopologyNetwork    `json:"networks"`
	Connections []TopologyConnection `json:"connections"`
}

func (m *Manager) labels(id string) map[string]string {
	return map[string]string{
		dockerx.LabelManaged:                  "true",
		"com.rousoftware.asgard.network-id":   id,
		"com.rousoftware.asgard.network-kind": "shared",
	}
}

func (m *Manager) Create(ctx context.Context, item store.ManagedNetwork) (store.ManagedNetwork, error) {
	if item.Driver == "" {
		item.Driver = "bridge"
	}
	if item.Driver != "bridge" {
		return store.ManagedNetwork{}, errors.New("only the bridge network driver is supported")
	}
	if err := m.Store.CreateManagedNetwork(ctx, item); err != nil {
		return store.ManagedNetwork{}, err
	}
	if err := m.Docker.EnsureManagedNetwork(ctx, item.DockerName, item.Internal, m.labels(item.ID)); err != nil {
		_, _ = m.Store.DB.ExecContext(context.WithoutCancel(ctx), `DELETE FROM managed_networks WHERE id=?`, item.ID)
		return store.ManagedNetwork{}, err
	}
	return m.Store.GetManagedNetwork(ctx, item.ID)
}

func (m *Manager) Attach(ctx context.Context, networkID, serviceID, requestedAlias string) (store.ManagedNetwork, error) {
	network, err := m.Store.GetManagedNetwork(ctx, networkID)
	if err != nil {
		return store.ManagedNetwork{}, err
	}
	service, err := m.Store.GetService(ctx, serviceID)
	if err != nil {
		return store.ManagedNetwork{}, err
	}
	project, err := m.Store.GetProject(ctx, service.ProjectID)
	if err != nil {
		return store.ManagedNetwork{}, err
	}
	alias := NormalizeAlias(requestedAlias)
	if alias == "" {
		alias = DefaultAlias(project.Slug, service.Name)
	}
	if err := m.Store.AddNetworkMember(ctx, network.ID, project.ID, service.ID, alias); err != nil {
		return store.ManagedNetwork{}, err
	}
	rollback := func() { _ = m.Store.RemoveNetworkMember(context.WithoutCancel(ctx), network.ID, service.ID) }
	if err := m.Docker.EnsureManagedNetwork(ctx, network.DockerName, network.Internal, m.labels(network.ID)); err != nil {
		rollback()
		return store.ManagedNetwork{}, err
	}
	if service.Runtime != nil {
		aliases := []string{service.Runtime.DockerName, alias}
		if err := m.Docker.ConnectNetworkAliases(ctx, network.DockerName, service.Runtime.DockerID, aliases); err != nil {
			rollback()
			return store.ManagedNetwork{}, err
		}
	}
	return m.Store.GetManagedNetwork(ctx, network.ID)
}

func (m *Manager) Detach(ctx context.Context, networkID, serviceID string) (store.ManagedNetwork, error) {
	network, err := m.Store.GetManagedNetwork(ctx, networkID)
	if err != nil {
		return store.ManagedNetwork{}, err
	}
	member, err := m.Store.GetNetworkMember(ctx, network.ID, serviceID)
	if err != nil {
		return store.ManagedNetwork{}, err
	}
	disconnected := false
	if member.DockerID != "" {
		if _, inspectErr := m.Docker.InspectNetwork(ctx, network.DockerName); inspectErr == nil {
			if err := m.Docker.DisconnectNetwork(ctx, network.DockerName, member.DockerID); err != nil {
				return store.ManagedNetwork{}, err
			}
			disconnected = true
		}
	}
	if err := m.Store.RemoveNetworkMember(ctx, network.ID, serviceID); err != nil {
		if disconnected {
			_ = m.Docker.ConnectNetworkAliases(context.WithoutCancel(ctx), network.DockerName, member.DockerID, []string{member.DockerName, member.Alias})
		}
		return store.ManagedNetwork{}, err
	}
	return m.Store.GetManagedNetwork(ctx, network.ID)
}

func (m *Manager) Reconcile(ctx context.Context, networkID string) (store.ManagedNetwork, error) {
	network, err := m.Store.GetManagedNetwork(ctx, networkID)
	if err != nil {
		return store.ManagedNetwork{}, err
	}
	if err := m.Docker.EnsureManagedNetwork(ctx, network.DockerName, network.Internal, m.labels(network.ID)); err != nil {
		return store.ManagedNetwork{}, err
	}
	for _, member := range network.Members {
		if member.DockerID == "" {
			continue
		}
		if err := m.Docker.ConnectNetworkAliases(ctx, network.DockerName, member.DockerID, []string{member.DockerName, member.Alias}); err != nil {
			return store.ManagedNetwork{}, fmt.Errorf("attach %s/%s: %w", member.ProjectSlug, member.ServiceName, err)
		}
	}
	return m.Store.GetManagedNetwork(ctx, network.ID)
}

func (m *Manager) ReconcileAll(ctx context.Context) error {
	items, err := m.Store.ListManagedNetworks(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range items {
		if _, err := m.Reconcile(ctx, item.ID); err != nil {
			failures = append(failures, fmt.Errorf("reconcile network %s: %w", item.Name, err))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) Delete(ctx context.Context, networkID string) error {
	network, err := m.Store.GetManagedNetwork(ctx, networkID)
	if err != nil {
		return err
	}
	if len(network.Members) > 0 {
		return fmt.Errorf("disconnect all %d service(s) before deleting this network", len(network.Members))
	}
	if err := m.Docker.RemoveManagedNetwork(ctx, network.DockerName, network.ID); err != nil {
		return err
	}
	return m.Store.DeleteManagedNetwork(ctx, network.ID)
}

func (m *Manager) List(ctx context.Context) ([]NetworkView, error) {
	items, err := m.Store.ListManagedNetworks(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]NetworkView, 0, len(items))
	for _, item := range items {
		views = append(views, m.view(ctx, item))
	}
	return views, nil
}

func (m *Manager) Get(ctx context.Context, idOrSlug string) (NetworkView, error) {
	item, err := m.Store.GetManagedNetwork(ctx, idOrSlug)
	if err != nil {
		return NetworkView{}, err
	}
	return m.view(ctx, item), nil
}

func (m *Manager) view(ctx context.Context, item store.ManagedNetwork) NetworkView {
	view := NetworkView{ManagedNetwork: item, Status: "missing"}
	runtime, runtimeErr := m.Docker.InspectNetwork(ctx, item.DockerName)
	if runtimeErr != nil {
		view.RuntimeError = runtimeErr.Error()
		return view
	}
	view.Status = "active"
	view.Runtime = &runtime
	for index := range view.Members {
		if endpoint, ok := endpointFor(runtime, view.Members[index].DockerID); ok {
			view.Members[index].Connected = true
			view.Members[index].IPv4Address = endpoint.IPv4Address
		}
	}
	return view
}

func (m *Manager) Topology(ctx context.Context) (Topology, error) {
	projects, err := m.Store.ListProjects(ctx)
	if err != nil {
		return Topology{}, err
	}
	shared, err := m.List(ctx)
	if err != nil {
		return Topology{}, err
	}
	topology := Topology{Projects: []TopologyProject{}, Services: []TopologyService{}, Networks: []TopologyNetwork{}, Connections: []TopologyConnection{}}
	edgeRuntime, _ := m.Docker.InspectNetwork(ctx, m.EdgeNetwork)
	edgeMembers := 0
	for _, project := range projects {
		topology.Projects = append(topology.Projects, TopologyProject{ID: project.ID, Slug: project.Slug, Name: project.Name, Status: project.Status, ServiceCount: len(project.Services)})
		privateID := "project:" + project.ID
		privateName := "asgard-project-" + project.Slug
		privateRuntime, privateErr := m.Docker.InspectNetwork(ctx, privateName)
		privateStatus := "active"
		if privateErr != nil {
			privateStatus = "pending"
		}
		topology.Networks = append(topology.Networks, topologyNetwork(privateID, "project", project.Name+" private", privateName, "Project-isolated service discovery", privateStatus, false, len(project.Services), privateRuntime))
		for _, service := range project.Services {
			state := "not deployed"
			dockerName := ""
			dockerID := ""
			if service.Runtime != nil {
				state, dockerName, dockerID = service.Runtime.State, service.Runtime.DockerName, service.Runtime.DockerID
			}
			topology.Services = append(topology.Services, TopologyService{ID: service.ID, ProjectID: project.ID, ProjectSlug: project.Slug, Name: service.Name, Role: service.Role, State: state, DockerName: dockerName, Public: service.Public, Hostname: service.Hostname})
			status, ip := connectionState(privateRuntime, dockerID, state)
			topology.Connections = append(topology.Connections, TopologyConnection{ID: privateID + ":" + service.ID, Kind: "project", NetworkID: privateID, ProjectID: project.ID, ServiceID: service.ID, Alias: service.Name, Status: status, IPv4Address: ip})
			if service.Public {
				edgeMembers++
				status, ip = connectionState(edgeRuntime, dockerID, state)
				topology.Connections = append(topology.Connections, TopologyConnection{ID: "edge:" + service.ID, Kind: "edge", NetworkID: "edge", ProjectID: project.ID, ServiceID: service.ID, Alias: service.Hostname, Status: status, IPv4Address: ip})
			}
		}
	}
	edgeStatus := "missing"
	if edgeRuntime.ID != "" {
		edgeStatus = "active"
	}
	topology.Networks = append([]TopologyNetwork{topologyNetwork("edge", "edge", "Public edge", m.EdgeNetwork, "Traefik ingress and HTTPS routes", edgeStatus, false, edgeMembers, edgeRuntime)}, topology.Networks...)
	for _, view := range shared {
		runtime := dockerx.NetworkDetails{}
		if view.Runtime != nil {
			runtime = *view.Runtime
		}
		topology.Networks = append(topology.Networks, topologyNetwork("shared:"+view.ID, "shared", view.Name, view.DockerName, view.Description, view.Status, view.Internal, len(view.Members), runtime))
		for _, member := range view.Members {
			status := "pending"
			if member.DockerID != "" {
				if member.Connected {
					status = "connected"
				} else {
					status = "disconnected"
				}
			}
			topology.Connections = append(topology.Connections, TopologyConnection{ID: "shared:" + view.ID + ":" + member.ServiceID, Kind: "shared", NetworkID: "shared:" + view.ID, ProjectID: member.ProjectID, ServiceID: member.ServiceID, Alias: member.Alias, Status: status, IPv4Address: member.IPv4Address})
		}
	}
	sort.Slice(topology.Networks, func(i, j int) bool {
		if topology.Networks[i].Kind != topology.Networks[j].Kind {
			return networkKindOrder(topology.Networks[i].Kind) < networkKindOrder(topology.Networks[j].Kind)
		}
		return topology.Networks[i].Name < topology.Networks[j].Name
	})
	return topology, nil
}

func topologyNetwork(id, kind, name, dockerName, description, status string, internal bool, members int, runtime dockerx.NetworkDetails) TopologyNetwork {
	driver := runtime.Driver
	if driver == "" {
		driver = "bridge"
	}
	return TopologyNetwork{ID: id, Kind: kind, Name: name, DockerName: dockerName, Description: description, Status: status, Driver: driver, Internal: internal, MemberCount: members, Subnets: runtime.Subnets}
}

func connectionState(runtime dockerx.NetworkDetails, dockerID, serviceState string) (string, string) {
	if dockerID == "" {
		return "pending", ""
	}
	if endpoint, ok := endpointFor(runtime, dockerID); ok {
		return "connected", endpoint.IPv4Address
	}
	if serviceState != "running" {
		return "stopped", ""
	}
	return "disconnected", ""
}

func endpointFor(runtime dockerx.NetworkDetails, dockerID string) (dockerx.NetworkEndpoint, bool) {
	if dockerID == "" {
		return dockerx.NetworkEndpoint{}, false
	}
	for _, endpoint := range runtime.Endpoints {
		if endpoint.ContainerID == dockerID || strings.HasPrefix(endpoint.ContainerID, dockerID) || strings.HasPrefix(dockerID, endpoint.ContainerID) {
			return endpoint, true
		}
	}
	return dockerx.NetworkEndpoint{}, false
}

func DefaultAlias(projectSlug, serviceName string) string {
	project := NormalizeAlias(projectSlug)
	service := NormalizeAlias(serviceName)
	value := strings.Trim(project+"--"+service, "-")
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}

func NormalizeAlias(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	dash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out.WriteRune(r)
			dash = false
		} else if (r == '-' || r == '.' || r == '_' || unicode.IsSpace(r)) && out.Len() > 0 && !dash {
			out.WriteByte('-')
			dash = true
		}
	}
	result := strings.Trim(out.String(), "-")
	if len(result) > 63 {
		result = strings.TrimRight(result[:63], "-")
	}
	return result
}

func networkKindOrder(kind string) int {
	switch kind {
	case "edge":
		return 0
	case "shared":
		return 1
	default:
		return 2
	}
}
