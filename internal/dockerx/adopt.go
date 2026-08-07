package dockerx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type AdoptionInspection struct {
	Container       Container `json:"container"`
	Safe            bool      `json:"safe"`
	Blockers        []string  `json:"blockers"`
	EnvironmentKeys []string  `json:"environmentKeys"`
	NamedVolumes    []Mount   `json:"namedVolumes"`
	BriefDowntime   bool      `json:"briefDowntime"`
}
type AdoptionRequest struct{ ProjectID, ProjectSlug, ServiceID, ServiceName, ReleaseID string }
type AdoptionResult struct {
	Replacement          Container         `json:"replacement"`
	OriginalID           string            `json:"originalId"`
	OriginalName         string            `json:"originalName"`
	OriginalRetainedName string            `json:"originalRetainedName"`
	RollbackUntil        time.Time         `json:"rollbackUntil"`
	Environment          map[string]string `json:"-"`
	Command              []string          `json:"-"`
	Volumes              []string          `json:"-"`
	Image                string            `json:"-"`
	Memory               int64             `json:"-"`
	NanoCPUs             int64             `json:"-"`
	PIDs                 int64             `json:"-"`
}

func (e *Engine) InspectAdoption(ctx context.Context, id string) (AdoptionInspection, error) {
	result, err := e.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{Size: true})
	if err != nil {
		return AdoptionInspection{}, err
	}
	summary, err := e.Container(ctx, id)
	if err != nil {
		return AdoptionInspection{}, err
	}
	item := result.Container
	blockers := []string{}
	host := item.HostConfig
	if host.Privileged {
		blockers = append(blockers, "Privileged mode is not supported")
	}
	if host.NetworkMode == "host" || strings.HasPrefix(string(host.NetworkMode), "container:") {
		blockers = append(blockers, "Host or container network mode is not supported")
	}
	if host.PidMode != "" || host.IpcMode == "host" {
		blockers = append(blockers, "Shared PID or host IPC namespace is not supported")
	}
	if len(host.Devices) > 0 {
		blockers = append(blockers, "Host devices are not supported")
	}
	for _, bind := range host.Binds {
		blockers = append(blockers, "Host bind mount is not supported: "+maskPath(bind))
	}
	named := []Mount{}
	for _, m := range item.Mounts {
		if m.Type == mount.TypeBind {
			blockers = append(blockers, "Host bind mount is not supported: "+maskPath(m.Source))
		}
		if m.Type == mount.TypeVolume {
			named = append(named, Mount{Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
		}
		if strings.Contains(strings.ToLower(m.Destination), "docker.sock") {
			blockers = append(blockers, "Docker socket mounts cannot be adopted")
		}
	}
	keys := []string{}
	for _, entry := range item.Config.Env {
		keys = append(keys, strings.SplitN(entry, "=", 2)[0])
	}
	return AdoptionInspection{Container: summary, Safe: len(blockers) == 0, Blockers: blockers, EnvironmentKeys: keys, NamedVolumes: named, BriefDowntime: true}, nil
}

func maskPath(value string) string {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) > 0 && len(parts[0]) > 1 {
		parts[0] = "[host path]"
	}
	return strings.Join(parts, ":")
}

func (e *Engine) Adopt(ctx context.Context, id string, request AdoptionRequest) (AdoptionResult, error) {
	inspection, err := e.InspectAdoption(ctx, id)
	if err != nil {
		return AdoptionResult{}, err
	}
	if !inspection.Safe {
		return AdoptionResult{}, fmt.Errorf("container cannot be adopted: %s", strings.Join(inspection.Blockers, "; "))
	}
	raw, err := e.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return AdoptionResult{}, err
	}
	item := raw.Container
	originalName := strings.TrimPrefix(item.Name, "/")
	retainedName := originalName + "-asgard-original-" + time.Now().UTC().Format("20060102t150405")
	wasRunning := item.State != nil && item.State.Running
	if wasRunning {
		timeout := 15
		if _, err = e.client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			return AdoptionResult{}, err
		}
	}
	if _, err = e.client.ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: retainedName}); err != nil {
		if wasRunning {
			_, _ = e.client.ContainerStart(context.WithoutCancel(ctx), id, client.ContainerStartOptions{})
		}
		return AdoptionResult{}, err
	}
	rollback := func() {
		_, _ = e.client.ContainerRename(context.Background(), id, client.ContainerRenameOptions{NewName: originalName})
		if wasRunning {
			_, _ = e.client.ContainerStart(context.Background(), id, client.ContainerStartOptions{})
		}
	}
	configCopy := *item.Config
	hostCopy := *item.HostConfig
	labels := map[string]string{}
	for key, value := range configCopy.Labels {
		labels[key] = value
	}
	labels[LabelManaged] = "true"
	labels["com.rousoftware.asgard.project-id"] = request.ProjectID
	labels["com.rousoftware.asgard.project"] = request.ProjectSlug
	labels["com.rousoftware.asgard.service-id"] = request.ServiceID
	labels["com.rousoftware.asgard.service"] = request.ServiceName
	labels["com.rousoftware.asgard.adopted-from"] = id
	configCopy.Labels = labels
	hostCopy.AutoRemove = false
	hostCopy.Privileged = false
	hostCopy.CapDrop = appendUnique(hostCopy.CapDrop, "ALL")
	hostCopy.SecurityOpt = appendUnique(hostCopy.SecurityOpt, "no-new-privileges:true")
	networking := item.NetworkSettings.Networks
	created, err := e.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &configCopy, HostConfig: &hostCopy, NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: networking}, Name: originalName})
	if err != nil {
		rollback()
		return AdoptionResult{}, err
	}
	fail := func(cause error) (AdoptionResult, error) {
		_, _ = e.client.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		rollback()
		return AdoptionResult{}, cause
	}
	if _, err = e.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fail(err)
	}
	if err = e.WaitReady(ctx, created.ID, 60*time.Second, func(string) {}); err != nil {
		return fail(err)
	}
	replacement, err := e.Container(ctx, created.ID)
	if err != nil {
		return fail(err)
	}
	environment := map[string]string{}
	for _, entry := range configCopy.Env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			environment[parts[0]] = parts[1]
		}
	}
	volumes := []string{}
	for _, m := range inspection.NamedVolumes {
		mode := "rw"
		if !m.RW {
			mode = "ro"
		}
		volumes = append(volumes, m.Name+":"+m.Destination+":"+mode)
	}
	pids := int64(256)
	if hostCopy.PidsLimit != nil {
		pids = *hostCopy.PidsLimit
	}
	return AdoptionResult{Replacement: replacement, OriginalID: id, OriginalName: originalName, OriginalRetainedName: retainedName, RollbackUntil: time.Now().UTC().Add(24 * time.Hour), Environment: environment, Command: configCopy.Cmd, Volumes: volumes, Image: configCopy.Image, Memory: hostCopy.Memory, NanoCPUs: hostCopy.NanoCPUs, PIDs: pids}, nil
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func (e *Engine) RollbackAdoption(ctx context.Context, result AdoptionResult) error {
	_, _ = e.client.ContainerRemove(ctx, result.Replacement.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := e.client.ContainerRename(ctx, result.OriginalID, client.ContainerRenameOptions{NewName: result.OriginalName}); err != nil {
		return err
	}
	_, err := e.client.ContainerStart(ctx, result.OriginalID, client.ContainerStartOptions{})
	return err
}
func (e *Engine) ConnectNetwork(ctx context.Context, networkName, containerID, alias string) error {
	return e.ConnectNetworkAliases(ctx, networkName, containerID, []string{alias})
}

func (e *Engine) ConnectNetworkAliases(ctx context.Context, networkName, containerID string, aliases []string) error {
	inspection, err := e.client.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	for id := range inspection.Network.Containers {
		if id == containerID || strings.HasPrefix(id, containerID) || strings.HasPrefix(containerID, id) {
			return nil
		}
	}
	clean := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias = strings.TrimSpace(alias); alias != "" {
			clean = appendUnique(clean, alias)
		}
	}
	_, err = e.client.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{Container: containerID, EndpointConfig: &network.EndpointSettings{Aliases: clean}})
	return err
}

func (e *Engine) DisconnectNetwork(ctx context.Context, networkName, containerID string) error {
	inspection, err := e.client.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	connected := false
	for id := range inspection.Network.Containers {
		if id == containerID || strings.HasPrefix(id, containerID) || strings.HasPrefix(containerID, id) {
			connected = true
			break
		}
	}
	if !connected {
		return nil
	}
	_, err = e.client.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{Container: containerID, Force: false})
	return err
}
