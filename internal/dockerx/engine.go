package dockerx

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

const LabelManaged = "com.rousoftware.asgard.managed"

type Engine struct{ client *client.Client }

type Container struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	ImageID   string            `json:"imageId"`
	State     string            `json:"state"`
	Status    string            `json:"status"`
	Health    string            `json:"health"`
	Restarts  int               `json:"restarts"`
	CreatedAt time.Time         `json:"createdAt"`
	SizeRW    int64             `json:"sizeRw"`
	Labels    map[string]string `json:"labels"`
	Managed   bool              `json:"managed"`
	ProjectID string            `json:"projectId,omitempty"`
	ServiceID string            `json:"serviceId,omitempty"`
	ReleaseID string            `json:"releaseId,omitempty"`
	Ports     []string          `json:"ports"`
	Mounts    []Mount           `json:"mounts"`
}

type Mount struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

type Host struct {
	Available         bool     `json:"available"`
	Error             string   `json:"error,omitempty"`
	Name              string   `json:"name"`
	DockerVersion     string   `json:"dockerVersion"`
	OperatingSystem   string   `json:"operatingSystem"`
	KernelVersion     string   `json:"kernelVersion"`
	Architecture      string   `json:"architecture"`
	CPUs              int      `json:"cpus"`
	MemoryBytes       int64    `json:"memoryBytes"`
	Containers        int      `json:"containers"`
	ContainersRunning int      `json:"containersRunning"`
	Images            int      `json:"images"`
	StorageDriver     string   `json:"storageDriver"`
	CgroupVersion     string   `json:"cgroupVersion"`
	LoggingDriver     string   `json:"loggingDriver"`
	SecurityOptions   []string `json:"securityOptions"`
}

type Stats struct {
	CPUPercent  float64 `json:"cpuPercent"`
	MemoryBytes int64   `json:"memoryBytes"`
	MemoryLimit int64   `json:"memoryLimit"`
	NetworkRX   int64   `json:"networkRx"`
	NetworkTX   int64   `json:"networkTx"`
	BlockRead   int64   `json:"blockRead"`
	BlockWrite  int64   `json:"blockWrite"`
	PIDs        int64   `json:"pids"`

	// CFS throttling counters, cumulative since the container started.
	//
	// Average CPU is the wrong statistic for a bursty request-serving workload
	// under a quota: rendering happens in sub-second bursts that hit the
	// ceiling and then idle, and any sampling window long enough to be cheap
	// averages them into noise. A service can report 0.01% CPU while spending
	// 42% of its scheduling periods stopped at the quota. These counters say so
	// directly, and Docker has always returned them — Asgard simply dropped
	// them.
	CPUPeriods          int64 `json:"cpuPeriods"`
	CPUThrottledPeriods int64 `json:"cpuThrottledPeriods"`
	CPUThrottledNanos   int64 `json:"cpuThrottledNanos"`
	// ThrottledPercent is the share of scheduling periods throttled over the
	// container's whole life. The collector replaces it with the share over the
	// last sampling interval, which is what actually answers "is it throttled
	// right now"; a lifetime figure only ever climbs slowly away from the truth.
	ThrottledPercent float64 `json:"throttledPercent"`

	CollectedAt time.Time `json:"collectedAt"`
}

type NetworkEndpoint struct {
	ContainerID string `json:"containerId"`
	Name        string `json:"name"`
	EndpointID  string `json:"endpointId"`
	IPv4Address string `json:"ipv4Address,omitempty"`
	IPv6Address string `json:"ipv6Address,omitempty"`
	MACAddress  string `json:"macAddress,omitempty"`
}

type NetworkDetails struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope"`
	Internal   bool              `json:"internal"`
	Attachable bool              `json:"attachable"`
	Subnets    []string          `json:"subnets"`
	Gateways   []string          `json:"gateways"`
	Labels     map[string]string `json:"labels"`
	Endpoints  []NetworkEndpoint `json:"endpoints"`
}

func New(host string) (*Engine, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	return &Engine{client: cli}, nil
}

func (e *Engine) Close() error { return e.client.Close() }

func (e *Engine) Ping(ctx context.Context) error {
	_, err := e.client.Ping(ctx, client.PingOptions{})
	return err
}

func (e *Engine) Host(ctx context.Context) Host {
	result, err := e.client.Info(ctx, client.InfoOptions{})
	if err != nil {
		return Host{Available: false, Error: err.Error()}
	}
	info := result.Info
	return Host{Available: true, Name: info.Name, DockerVersion: info.ServerVersion, OperatingSystem: info.OperatingSystem, KernelVersion: info.KernelVersion, Architecture: info.Architecture, CPUs: info.NCPU, MemoryBytes: info.MemTotal, Containers: info.Containers, ContainersRunning: info.ContainersRunning, Images: info.Images, StorageDriver: info.Driver, CgroupVersion: info.CgroupVersion, LoggingDriver: info.LoggingDriver, SecurityOptions: info.SecurityOptions}
}

func (e *Engine) Containers(ctx context.Context, all bool) ([]Container, error) {
	result, err := e.client.ContainerList(ctx, client.ContainerListOptions{All: all, Size: true})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(result.Items))
	for _, item := range result.Items {
		name := strings.TrimPrefix(first(item.Names), "/")
		health := ""
		if item.Health != nil {
			health = string(item.Health.Status)
		}
		ports := []string{}
		for _, port := range item.Ports {
			if port.PublicPort > 0 {
				ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", port.IP, port.PublicPort, port.PrivatePort, port.Type))
			} else {
				ports = append(ports, fmt.Sprintf("%d/%s", port.PrivatePort, port.Type))
			}
		}
		mounts := []Mount{}
		for _, m := range item.Mounts {
			mounts = append(mounts, Mount{Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
		}
		labels := item.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, Container{ID: item.ID, Name: name, Image: item.Image, ImageID: item.ImageID, State: string(item.State), Status: item.Status, Health: health, CreatedAt: time.Unix(item.Created, 0).UTC(), SizeRW: item.SizeRw, Labels: labels, Managed: labels[LabelManaged] == "true", ProjectID: labels["com.rousoftware.asgard.project-id"], ServiceID: labels["com.rousoftware.asgard.service-id"], ReleaseID: labels["com.rousoftware.asgard.release-id"], Ports: ports, Mounts: mounts})
	}
	return out, nil
}

func first(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func (e *Engine) Stats(ctx context.Context, id string) (Stats, error) {
	result, err := e.client.ContainerStats(ctx, id, client.ContainerStatsOptions{Stream: false, IncludePreviousSample: true})
	if err != nil {
		return Stats{}, err
	}
	defer result.Body.Close()
	var raw container.StatsResponse
	if err := json.NewDecoder(result.Body).Decode(&raw); err != nil {
		return Stats{}, err
	}
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage - raw.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(raw.CPUStats.SystemUsage - raw.PreCPUStats.SystemUsage)
	cpus := float64(raw.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
	}
	cpu := 0.0
	if systemDelta > 0 && cpuDelta >= 0 {
		cpu = (cpuDelta / systemDelta) * cpus * 100
	}
	memory := raw.MemoryStats.Usage
	if cache, ok := raw.MemoryStats.Stats["inactive_file"]; ok && memory > cache {
		memory -= cache
	}
	var rx, tx int64
	for _, item := range raw.Networks {
		rx += int64(item.RxBytes)
		tx += int64(item.TxBytes)
	}
	var read, write int64
	for _, item := range raw.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(item.Op) {
		case "read":
			read += int64(item.Value)
		case "write":
			write += int64(item.Value)
		}
	}
	throttle := raw.CPUStats.ThrottlingData
	stats := Stats{
		CPUPercent:          cpu,
		MemoryBytes:         int64(memory),
		MemoryLimit:         int64(raw.MemoryStats.Limit),
		NetworkRX:           rx,
		NetworkTX:           tx,
		BlockRead:           read,
		BlockWrite:          write,
		PIDs:                int64(raw.PidsStats.Current),
		CPUPeriods:          int64(throttle.Periods),
		CPUThrottledPeriods: int64(throttle.ThrottledPeriods),
		CPUThrottledNanos:   int64(throttle.ThrottledTime),
		CollectedAt:         time.Now().UTC(),
	}
	stats.ThrottledPercent = ThrottledPercent(stats.CPUPeriods, stats.CPUThrottledPeriods)
	return stats, nil
}

// ThrottledPercent expresses throttled periods as a share of total periods.
// Periods is zero when the container has no CPU quota at all, which is not the
// same as never being throttled, so the answer there is zero rather than a
// divide by zero.
func ThrottledPercent(periods, throttled int64) float64 {
	if periods <= 0 || throttled <= 0 {
		return 0
	}
	if throttled > periods {
		throttled = periods
	}
	return float64(throttled) / float64(periods) * 100
}

func (e *Engine) Logs(ctx context.Context, id string, tail int, since string) (string, error) {
	if tail <= 0 || tail > 5000 {
		tail = 500
	}
	result, err := e.client.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Timestamps: true, Tail: strconv.Itoa(tail), Since: since})
	if err != nil {
		return "", err
	}
	defer result.Close()
	raw, err := io.ReadAll(io.LimitReader(result, 10<<20))
	if err != nil {
		return "", err
	}
	return demux(raw), nil
}

func demux(raw []byte) string {
	var out bytes.Buffer
	for len(raw) >= 8 && (raw[0] == 1 || raw[0] == 2) {
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		if size < 0 || 8+size > len(raw) {
			break
		}
		out.Write(raw[8 : 8+size])
		raw = raw[8+size:]
	}
	if out.Len() == 0 {
		return string(raw)
	}
	out.Write(raw)
	return out.String()
}

func (e *Engine) Action(ctx context.Context, id, action string) error {
	timeout := 15
	switch action {
	case "start":
		_, err := e.client.ContainerStart(ctx, id, client.ContainerStartOptions{})
		return err
	case "stop":
		_, err := e.client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
		return err
	case "restart":
		_, err := e.client.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &timeout})
		return err
	default:
		return fmt.Errorf("unsupported container action %q", action)
	}
}

func (e *Engine) EnsureNetwork(ctx context.Context, name string, internal bool, labels map[string]string) error {
	result, err := e.client.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("name", name)})
	if err != nil {
		return err
	}
	for _, n := range result.Items {
		if n.Name == name {
			return nil
		}
	}
	_, err = e.client.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: internal, Attachable: true, Labels: labels})
	return err
}

func (e *Engine) EnsureManagedNetwork(ctx context.Context, name string, internal bool, labels map[string]string) error {
	result, err := e.client.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("name", name)})
	if err != nil {
		return err
	}
	for _, item := range result.Items {
		if item.Name != name {
			continue
		}
		if item.Labels[LabelManaged] != "true" {
			return fmt.Errorf("network %s already exists and is not managed by Asgard", name)
		}
		for key, value := range labels {
			if item.Labels[key] != value {
				return fmt.Errorf("network %s has conflicting ownership metadata", name)
			}
		}
		if item.Internal != internal {
			return fmt.Errorf("network %s has a different internal-access policy", name)
		}
		if item.Driver != "bridge" {
			return fmt.Errorf("network %s uses unsupported driver %s", name, item.Driver)
		}
		return nil
	}
	_, err = e.client.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: internal, Attachable: true, Labels: labels})
	return err
}

func (e *Engine) InspectNetwork(ctx context.Context, name string) (NetworkDetails, error) {
	result, err := e.client.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return NetworkDetails{}, err
	}
	item := result.Network
	details := NetworkDetails{ID: item.ID, Name: item.Name, Driver: item.Driver, Scope: item.Scope, Internal: item.Internal, Attachable: item.Attachable, Labels: item.Labels, Subnets: []string{}, Gateways: []string{}, Endpoints: []NetworkEndpoint{}}
	for _, config := range item.IPAM.Config {
		if config.Subnet.IsValid() {
			details.Subnets = append(details.Subnets, config.Subnet.String())
		}
		if config.Gateway.IsValid() {
			details.Gateways = append(details.Gateways, config.Gateway.String())
		}
	}
	for containerID, endpoint := range item.Containers {
		value := NetworkEndpoint{ContainerID: containerID, Name: endpoint.Name, EndpointID: endpoint.EndpointID, MACAddress: endpoint.MacAddress.String()}
		if endpoint.IPv4Address.IsValid() {
			value.IPv4Address = endpoint.IPv4Address.String()
		}
		if endpoint.IPv6Address.IsValid() {
			value.IPv6Address = endpoint.IPv6Address.String()
		}
		details.Endpoints = append(details.Endpoints, value)
	}
	sort.Slice(details.Endpoints, func(i, j int) bool { return details.Endpoints[i].Name < details.Endpoints[j].Name })
	return details, nil
}

func (e *Engine) RemoveManagedNetwork(ctx context.Context, name, networkID string) error {
	result, err := e.client.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("name", name)})
	if err != nil {
		return err
	}
	for _, item := range result.Items {
		if item.Name != name {
			continue
		}
		if item.Labels[LabelManaged] != "true" || item.Labels["com.rousoftware.asgard.network-id"] != networkID {
			return fmt.Errorf("refusing to remove network %s without matching Asgard ownership labels", name)
		}
		_, err = e.client.NetworkRemove(ctx, item.ID, client.NetworkRemoveOptions{})
		return err
	}
	return nil
}

func (e *Engine) RemoveProjectNetwork(ctx context.Context, name, projectID string) error {
	result, err := e.client.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("name", name)})
	if err != nil {
		return err
	}
	for _, item := range result.Items {
		if item.Name != name {
			continue
		}
		if item.Labels[LabelManaged] != "true" || item.Labels["com.rousoftware.asgard.project-id"] != projectID {
			return fmt.Errorf("refusing to remove network %s without matching Asgard ownership labels", name)
		}
		_, err = e.client.NetworkRemove(ctx, item.ID, client.NetworkRemoveOptions{})
		return err
	}
	return nil
}

func (e *Engine) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := e.client.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		return nil
	}
	_, err = e.client.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
	return err
}

func NormalizeImage(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "https://hub.docker.com/r/") {
		value = strings.TrimPrefix(value, "https://hub.docker.com/r/")
		value = strings.TrimSuffix(value, "/")
	} else if strings.HasPrefix(value, "https://hub.docker.com/_/") {
		value = "library/" + strings.Trim(strings.TrimPrefix(value, "https://hub.docker.com/_/"), "/")
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return "", err
	}
	if reference.IsNameOnly(named) {
		named = reference.TagNameOnly(named)
	}
	return named.String(), nil
}

func (e *Engine) Pull(ctx context.Context, image string, log func(string)) error {
	normalized, err := NormalizeImage(image)
	if err != nil {
		return err
	}
	response, err := e.client.ImagePull(ctx, normalized, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer response.Close()
	scanner := bufio.NewScanner(response)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		var message map[string]any
		if json.Unmarshal(scanner.Bytes(), &message) == nil {
			status := fmt.Sprint(message["status"])
			id := fmt.Sprint(message["id"])
			if status != "<nil>" && status != "" {
				if id != "<nil>" && id != "" {
					log(id + ": " + status)
				} else {
					log(status)
				}
			}
		}
	}
	return scanner.Err()
}

func (e *Engine) Build(ctx context.Context, contextDir, dockerfile, tag string, log func(string)) error {
	reader, writer := io.Pipe()
	go func() { err := writeBuildContext(writer, contextDir); _ = writer.CloseWithError(err) }()
	result, err := e.client.ImageBuild(ctx, reader, client.ImageBuildOptions{Tags: []string{tag}, Dockerfile: filepath.ToSlash(dockerfile), Remove: true, ForceRemove: true, PullParent: true, Labels: map[string]string{LabelManaged: "true"}})
	if err != nil {
		return err
	}
	defer result.Body.Close()
	scanner := bufio.NewScanner(result.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var buildErr error
	for scanner.Scan() {
		var message struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
			Status string `json:"status"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) == nil {
			for _, line := range strings.Split(strings.TrimSpace(message.Stream), "\n") {
				if line != "" {
					log(line)
				}
			}
			if message.Status != "" {
				log(message.Status)
			}
			if message.Error != "" {
				log(message.Error)
				buildErr = errors.New(message.Error)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return buildErr
}

func writeBuildContext(writer io.Writer, root string) error {
	archive := tar.NewWriter(writer)
	defer archive.Close()
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	var total int64
	return filepath.WalkDir(rootAbs, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "data") {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeDevice != 0 || info.Mode()&os.ModeNamedPipe != 0 {
			return fmt.Errorf("unsafe build-context entry %s", rel)
		}
		if info.Size() > 500<<20 {
			return fmt.Errorf("build-context file %s exceeds 500 MiB", rel)
		}
		total += info.Size()
		if total > 1<<30 {
			return errors.New("build context exceeds 1 GiB")
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(archive, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
}

type CreateRequest struct {
	Project        store.Project
	Service        store.Service
	ReleaseID      string
	Version        int
	Image          string
	ProjectNetwork string
	EdgeNetwork    string
	// DataVolume and SourceSubpath locate the project's imported source inside
	// Asgard's own data volume. Project-relative mounts are served from there
	// as volume subpaths, so a Compose file that mounts its own config or
	// secret file works without ever exposing a host path to the workload.
	DataVolume    string
	SourceSubpath string
}

func (e *Engine) CreateServiceContainer(ctx context.Context, req CreateRequest) (Container, error) {
	name := fmt.Sprintf("asgard-%s-%s-r%d", req.Project.Slug, normalizeName(req.Service.Name), req.Version)
	env := make([]string, 0, len(req.Service.Environment))
	for key, value := range req.Service.Environment {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	ports := network.PortSet{}
	if req.Service.Port > 0 {
		ports[network.MustParsePort(fmt.Sprintf("%d/tcp", req.Service.Port))] = struct{}{}
	}
	labels := map[string]string{LabelManaged: "true", "com.rousoftware.asgard.project-id": req.Project.ID, "com.rousoftware.asgard.project": req.Project.Slug, "com.rousoftware.asgard.service-id": req.Service.ID, "com.rousoftware.asgard.service": req.Service.Name, "com.rousoftware.asgard.release-id": req.ReleaseID, "com.rousoftware.asgard.version": strconv.Itoa(req.Version)}
	mounts, err := buildMounts(req)
	if err != nil {
		return Container{}, err
	}
	pids := req.Service.PIDsLimit
	initProcess := true
	hostConfig := &container.HostConfig{NetworkMode: container.NetworkMode(req.ProjectNetwork), RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(req.Service.RestartPolicy)}, Resources: container.Resources{Memory: req.Service.MemoryLimit, NanoCPUs: int64(req.Service.CPULimit * 1e9), PidsLimit: &pids}, Mounts: mounts, LogConfig: container.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "5"}}, Init: &initProcess, CapDrop: []string{"ALL"}, CapAdd: workloadCapabilities(), SecurityOpt: []string{"no-new-privileges:true"}}
	endpoints := map[string]*network.EndpointSettings{req.ProjectNetwork: {Aliases: []string{name, req.Service.Name}}}
	if req.Service.Public {
		endpoints[req.EdgeNetwork] = &network.EndpointSettings{Aliases: []string{name}}
	}
	for _, shared := range req.Service.Networks {
		endpoints[shared.DockerName] = &network.EndpointSettings{Aliases: []string{name, shared.Alias}}
	}
	createResult, err := e.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: req.Image, Env: env, Cmd: req.Service.Command, Labels: labels, ExposedPorts: ports}, HostConfig: hostConfig, NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: endpoints}, Name: name})
	if err != nil {
		return Container{}, err
	}
	if _, err = e.client.ContainerStart(ctx, createResult.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = e.client.ContainerRemove(ctx, createResult.ID, client.ContainerRemoveOptions{Force: true})
		return Container{}, err
	}
	return e.Container(ctx, createResult.ID)
}

func buildMounts(req CreateRequest) ([]mount.Mount, error) {
	mounts := []mount.Mount{}
	for _, spec := range req.Service.Volumes {
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			continue
		}
		readOnly := len(parts) == 3 && parts[2] == "ro"
		relative, isProjectMount := composecfg.ProjectMount(spec)
		if !isProjectMount {
			mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: parts[0], Target: parts[1], ReadOnly: readOnly})
			continue
		}
		if req.DataVolume == "" || req.SourceSubpath == "" {
			return nil, fmt.Errorf("service %s mounts %q from its project source, which requires the Asgard data volume", req.Service.Name, relative)
		}
		subpath := path.Join(req.SourceSubpath, relative)
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: req.DataVolume, Target: parts[1], ReadOnly: readOnly, VolumeOptions: &mount.VolumeOptions{Subpath: subpath}})
	}
	return mounts, nil
}

func workloadCapabilities() []string {
	// These capabilities let common images initialize owned directories and then
	// switch from root to their unprivileged runtime user. Administrative,
	// networking, tracing, and raw-socket capabilities remain unavailable.
	return []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"}
}

func normalizeName(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func (e *Engine) Container(ctx context.Context, id string) (Container, error) {
	result, err := e.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{Size: true})
	if err != nil {
		return Container{}, err
	}
	item := result.Container
	labels := item.Config.Labels
	health := ""
	if item.State != nil && item.State.Health != nil {
		health = string(item.State.Health.Status)
	}
	mounts := []Mount{}
	for _, m := range item.Mounts {
		mounts = append(mounts, Mount{Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
	}
	created, _ := time.Parse(time.RFC3339Nano, item.Created)
	state := ""
	status := ""
	if item.State != nil {
		state = string(item.State.Status)
		status = string(item.State.Status)
	}
	sizeRW := int64(0)
	if item.SizeRw != nil {
		sizeRW = *item.SizeRw
	}
	return Container{ID: item.ID, Name: strings.TrimPrefix(item.Name, "/"), Image: item.Config.Image, ImageID: item.Image, State: state, Status: status, Health: health, Restarts: item.RestartCount, CreatedAt: created, SizeRW: sizeRW, Labels: labels, Managed: labels[LabelManaged] == "true", ProjectID: labels["com.rousoftware.asgard.project-id"], ServiceID: labels["com.rousoftware.asgard.service-id"], ReleaseID: labels["com.rousoftware.asgard.release-id"], Mounts: mounts}, nil
}

const readinessStabilityWindow = 5 * time.Second

func (e *Engine) WaitReady(ctx context.Context, id string, timeout time.Duration, log func(string)) error {
	deadline := time.Now().Add(timeout)
	var stableSince time.Time
	for time.Now().Before(deadline) {
		containerInfo, err := e.Container(ctx, id)
		if err != nil {
			return err
		}
		ready, nextStableSince, err := evaluateReadiness(containerInfo, stableSince, time.Now())
		if err != nil {
			return err
		}
		stableSince = nextStableSince
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if containerInfo.Health == "" && !stableSince.IsZero() {
			log("Verifying container stability…")
		} else {
			log("Waiting for container health…")
		}
	}
	return errors.New("container did not become healthy before timeout")
}

func evaluateReadiness(item Container, stableSince, now time.Time) (bool, time.Time, error) {
	if item.Restarts > 0 {
		return false, time.Time{}, fmt.Errorf("container restarted %d time(s) before readiness", item.Restarts)
	}
	if item.State == "exited" || item.State == "dead" || item.State == "removing" {
		return false, time.Time{}, fmt.Errorf("container entered %s state", item.State)
	}
	if item.Health == "unhealthy" {
		return false, time.Time{}, errors.New("container health check failed")
	}
	if item.State != "running" {
		return false, time.Time{}, nil
	}
	if item.Health == "healthy" {
		return true, time.Time{}, nil
	}
	if item.Health != "" {
		return false, time.Time{}, nil
	}
	if stableSince.IsZero() {
		return false, now, nil
	}
	return now.Sub(stableSince) >= readinessStabilityWindow, stableSince, nil
}

func (e *Engine) Remove(ctx context.Context, id string, volumes bool) error {
	_, err := e.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: volumes})
	return err
}

func (e *Engine) RunHelper(ctx context.Context, image, name, script string, mounts []mount.Mount, log func(string)) error {
	initProcess := true
	created, err := e.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: image, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{script}, Labels: map[string]string{LabelManaged: "true", "com.rousoftware.asgard.helper": "true"}},
		HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: mounts, AutoRemove: false, Init: &initProcess, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}},
		Name:       name,
	})
	if err != nil {
		return err
	}
	defer func() {
		_, _ = e.client.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err = e.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return err
	}
	wait := e.client.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		return err
	case response := <-wait.Result:
		logs, _ := e.Logs(context.WithoutCancel(ctx), created.ID, 1000, "")
		for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
			if line != "" {
				log(line)
			}
		}
		if response.StatusCode != 0 {
			return fmt.Errorf("helper exited with status %d", response.StatusCode)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) HTTPHealth(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

func (e *Engine) WaitHTTPReady(ctx context.Context, url string, timeout time.Duration, log func(string)) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := e.HTTPHealth(ctx, url); err == nil {
			return nil
		} else {
			lastErr = err
		}
		log("Waiting for HTTP readiness…")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("health endpoint did not become ready before timeout: %w", lastErr)
	}
	return errors.New("health endpoint did not become ready before timeout")
}
