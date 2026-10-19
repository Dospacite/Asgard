package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/proxy"
	"github.com/rousoftware/asgard/internal/reclaim"
	"github.com/rousoftware/asgard/internal/store"
)

type Deployer struct {
	Store       *store.Store
	Docker      *dockerx.Engine
	Proxy       *proxy.Generator
	EdgeNetwork string
	// DataDir and DataVolume let project-relative mounts be resolved as
	// subpaths of Asgard's own data volume rather than host paths.
	DataDir    string
	DataVolume string
	// Reclaimer frees the images the previous releases of this project left
	// behind. A deployment is what creates them, so it is the natural place to
	// collect the ones that just aged out of the retention window.
	Reclaimer *reclaim.Reclaimer
	// BuildConcurrency caps how many of a release's images build at once.
	// Values below one build them one at a time.
	BuildConcurrency int
	locks            sync.Map
}
type Payload struct {
	Trigger           string `json:"trigger"`
	RollbackReleaseID string `json:"rollbackReleaseId,omitempty"`
}

func (d *Deployer) Handle(ctx context.Context, op store.Operation) error {
	lockAny, _ := d.locks.LoadOrStore(op.TargetID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	var payload Payload
	_ = json.Unmarshal(op.Payload, &payload)
	if payload.Trigger == "" {
		payload.Trigger = "manual"
	}
	return d.deploy(ctx, op, payload)
}

func (d *Deployer) deploy(ctx context.Context, op store.Operation, payload Payload) (retErr error) {
	project, err := d.Store.GetProject(ctx, op.TargetID)
	if err != nil {
		return err
	}
	services := project.Services
	composeBytes, err := os.ReadFile(filepath.Join(project.SourcePath, project.ComposePath))
	if err != nil {
		return err
	}
	sourceRevision := project.SourceRef
	if payload.RollbackReleaseID != "" {
		target, err := d.Store.GetRelease(ctx, payload.RollbackReleaseID)
		if err != nil {
			return err
		}
		if target.ProjectID != project.ID || target.Status != "succeeded" {
			return errors.New("rollback target must be a successful release from this project")
		}
		services = releaseToServices(target.Services)
		composeBytes = []byte(target.ComposeSnapshot)
		sourceRevision = "rollback:" + target.ID
		payload.Trigger = "rollback"
	}
	if len(services) == 0 {
		return errors.New("project has no services")
	}
	sourceSubpath, err := d.sourceSubpath(project)
	if err != nil {
		return err
	}
	for index := range services {
		services[index].Networks, err = d.Store.ListServiceNetworks(ctx, services[index].ID)
		if err != nil {
			return fmt.Errorf("load networks for %s: %w", services[index].Name, err)
		}
	}
	ordered, err := topological(services)
	if err != nil {
		return err
	}
	if err := d.checkHostnames(ctx, project.ID, services); err != nil {
		return err
	}
	release, deployment, err := d.Store.BeginRelease(ctx, project.ID, op.ID, payload.Trigger, sourceRevision, string(composeBytes), services)
	if err != nil {
		return err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = d.Store.FinishRelease(context.WithoutCancel(ctx), release.ID, deployment.ID, "failed", errorText(retErr))
		}
	}()
	log := func(message string) { _ = d.Store.LogOperation(context.WithoutCancel(ctx), op.ID, "info", message) }
	progress := func(value int, message string) {
		_ = d.Store.ProgressOperation(context.WithoutCancel(ctx), op.ID, value, message)
		log(message)
	}
	progress(5, "Preparing project and shared networks")
	projectNetwork := "asgard-project-" + project.Slug
	if err = d.Docker.EnsureNetwork(ctx, projectNetwork, false, map[string]string{dockerx.LabelManaged: "true", "com.rousoftware.asgard.project-id": project.ID}); err != nil {
		return err
	}
	if err = d.Docker.EnsureNetwork(ctx, d.EdgeNetwork, false, map[string]string{dockerx.LabelManaged: "true"}); err != nil {
		return err
	}
	preparedNetworks := map[string]bool{}
	for _, svc := range services {
		for _, shared := range svc.Networks {
			if preparedNetworks[shared.ID] {
				continue
			}
			labels := map[string]string{dockerx.LabelManaged: "true", "com.rousoftware.asgard.network-id": shared.ID, "com.rousoftware.asgard.network-kind": "shared"}
			if err = d.Docker.EnsureManagedNetwork(ctx, shared.DockerName, shared.Internal, labels); err != nil {
				return fmt.Errorf("prepare shared network %s: %w", shared.Name, err)
			}
			preparedNetworks[shared.ID] = true
		}
	}
	for _, svc := range services {
		for _, spec := range svc.Volumes {
			relative, isProjectMount := composecfg.ProjectMount(spec)
			if isProjectMount {
				// A path the project does not ship but mounts as a data
				// directory — `./data:/data`, the way an ordinary Compose file
				// persists state — is created here, in the project's own source
				// tree, which is what Compose would have done on the host. The
				// mount is then served from Asgard's data volume as a subpath.
				// A file the project does ship is left alone: creation is only
				// for the paths that are missing.
				if err = createMountDirectory(filepath.Join(project.SourcePath, filepath.FromSlash(relative))); err != nil {
					return fmt.Errorf("prepare mount %s for %s: %w", relative, svc.Name, err)
				}
				continue
			}
			parts := strings.Split(spec, ":")
			if len(parts) >= 2 {
				if err = d.Docker.EnsureVolume(ctx, parts[0], map[string]string{dockerx.LabelManaged: "true", "com.rousoftware.asgard.project-id": project.ID, "com.rousoftware.asgard.service-id": svc.ID}); err != nil {
					return err
				}
			}
		}
	}
	// Every image is built before any running container is touched, so a
	// failed build leaves the live release exactly as it was. Independent
	// images build side by side instead of each waiting for the previous
	// service to be built, started and found healthy.
	builtRefs := map[string]string{}
	if payload.RollbackReleaseID == "" {
		plan, refs := planBuilds(project.SourcePath, project.Slug, release.Version, ordered)
		if len(plan) > 0 {
			progress(10, fmt.Sprintf("Building %d image(s) for %d service(s)", len(plan), len(refs)))
			unhold := d.holdBuilds()
			err = runBuilds(ctx, plan, d.BuildConcurrency, d.Docker.Build, log)
			unhold()
			if err != nil {
				return err
			}
		}
		builtRefs = refs
	}
	old, err := d.Store.ActiveRuntimes(ctx, project.ID)
	if err != nil {
		return err
	}
	stoppedOld := []string{}
	candidates := map[string]dockerx.Container{}
	// kept marks the candidates that are the previous release's own running
	// containers, carried over unchanged. A failed release must leave them
	// running, not remove them with the containers it created.
	kept := map[string]bool{}
	defer func() {
		if !succeeded {
			for serviceID, item := range candidates {
				if kept[serviceID] {
					continue
				}
				if output, logsErr := d.Docker.Logs(context.WithoutCancel(ctx), item.ID, 200, ""); logsErr == nil {
					for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
						if line != "" {
							log(item.Name + ": " + line)
						}
					}
				} else {
					log("Could not collect failed candidate logs for " + item.Name + ": " + logsErr.Error())
				}
				_ = d.Docker.Remove(context.WithoutCancel(ctx), item.ID, false)
			}
			for _, id := range stoppedOld {
				_ = d.Docker.Action(context.WithoutCancel(ctx), id, "start")
			}
		}
	}()
	for index, svc := range ordered {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress(30+(index*35/len(ordered)), fmt.Sprintf("Preparing %s", svc.Name))
		imageRef := svc.Image
		if built, ok := builtRefs[svc.ID]; ok {
			imageRef = built
		} else {
			normalized, normErr := dockerx.NormalizeImage(imageRef)
			if normErr != nil {
				return fmt.Errorf("image %s: %w", svc.Name, normErr)
			}
			imageRef = normalized
			// A rollback restores the image the release ran, which the host
			// keeps for the retention window. Pulling it would be wrong twice
			// over: a tag Asgard built itself exists on no registry, and rolling
			// back is not a request for whatever the tag points at today.
			if payload.RollbackReleaseID != "" && d.Docker.ImageExists(ctx, imageRef) {
				log("Using the image this release ran: " + imageRef)
			} else if err = d.Docker.Pull(ctx, imageRef, log); err != nil {
				if payload.RollbackReleaseID != "" {
					return fmt.Errorf("pull %s: %w. The image this release ran is no longer on the host — releases whose images have been reclaimed cannot be restored; deploy the project again to build a new one", svc.Name, err)
				}
				return fmt.Errorf("pull %s: %w", svc.Name, err)
			}
		}
		if err = d.Store.SetReleaseServiceImage(ctx, release.ID, svc.ID, imageRef); err != nil {
			return err
		}
		request := dockerx.CreateRequest{Project: project, Service: svc, ReleaseID: release.ID, Version: release.Version, Image: imageRef, ProjectNetwork: projectNetwork, EdgeNetwork: d.EdgeNetwork, DataVolume: d.DataVolume, SourceSubpath: sourceSubpath}
		request.SpecHash = d.specHash(ctx, request)
		if current, ok := d.unchanged(ctx, svc, request.SpecHash, old[svc.ID]); ok {
			log(fmt.Sprintf("%s is unchanged; keeping its running container %s", svc.Name, current.Name))
			candidates[svc.ID] = current
			kept[svc.ID] = true
			continue
		}
		if stopPriorBeforeCandidate(svc) && old[svc.ID].DockerID != "" {
			if err = d.Docker.Action(ctx, old[svc.ID].DockerID, "stop"); err != nil {
				return fmt.Errorf("stop prior %s: %w", svc.Name, err)
			}
			stoppedOld = append(stoppedOld, old[svc.ID].DockerID)
		}
		candidate, err := d.Docker.CreateServiceContainer(ctx, request)
		if err != nil {
			return fmt.Errorf("create %s: %w", svc.Name, err)
		}
		candidates[svc.ID] = candidate
		readiness := dockerx.ReadinessFor(svc)
		if err = d.Docker.WaitReady(ctx, candidate.ID, readiness, log); err != nil {
			return fmt.Errorf("health %s: %w", svc.Name, err)
		}
		if svc.Public && svc.Port > 0 && svc.HealthPath != "" {
			endpoint := fmt.Sprintf("http://%s:%d%s", candidate.Name, svc.Port, svc.HealthPath)
			log("Checking HTTP readiness for " + svc.Name)
			if err = d.Docker.WaitHTTPReady(ctx, endpoint, readiness.Timeout, log); err != nil {
				return fmt.Errorf("HTTP health %s: %w", svc.Name, err)
			}
		}
	}
	progress(70, "Switching routes atomically")
	if err = d.switchRelease(ctx, project, services, release.ID, candidates, kept, old, log); err != nil {
		return err
	}
	progress(82, "Retiring the previous release")
	for _, runtime := range old {
		if candidate, ok := candidatesForDocker(candidates, runtime.DockerID); ok && candidate.ID == runtime.DockerID {
			continue
		}
		if runtime.DockerID != "" {
			if err := d.Docker.Remove(ctx, runtime.DockerID, false); err != nil {
				log("Could not retire " + runtime.DockerName + ": " + err.Error())
			}
		}
	}
	if err = d.Store.FinishRelease(ctx, release.ID, deployment.ID, "succeeded", ""); err != nil {
		return err
	}
	succeeded = true
	_ = d.Store.TrimReleases(ctx, project.ID, d.releasesToKeep())
	// Reclaim only once the previous release's containers are gone: before
	// that, their images are still referenced and cannot be freed, and the
	// time it takes was being reported as the route switch.
	if d.Reclaimer != nil {
		progress(90, "Reclaiming storage from aged-out releases")
		d.reclaim(ctx, log)
	}
	progress(100, releaseSummary(release.Version, ordered, kept))
	return nil
}

// minReleaseHistory is how many successful releases a project keeps at least.
const minReleaseHistory = 5

// releasesToKeep keeps at least as many releases as have images retained for
// rollback; trimming to fewer would delete rollback targets whose images the
// reclaimer was told to keep.
func (d *Deployer) releasesToKeep() int {
	if d.Reclaimer != nil && d.Reclaimer.Policy.KeepReleases > minReleaseHistory {
		return d.Reclaimer.Policy.KeepReleases
	}
	return minReleaseHistory
}

// releaseSummary names what a release actually replaced. "Release r21 is
// live" alone reads as success even when every container was carried over and
// the change the operator meant to ship never reached one.
func releaseSummary(version int, services []store.Service, kept map[string]bool) string {
	recreated, unchanged := []string{}, 0
	for _, svc := range services {
		if kept[svc.ID] {
			unchanged++
			continue
		}
		recreated = append(recreated, svc.Name)
	}
	switch {
	case len(recreated) == 0:
		return fmt.Sprintf("Release r%d is live; nothing changed, so all %d services kept their running containers", version, unchanged)
	case unchanged == 0:
		return fmt.Sprintf("Release r%d is live; recreated all %d services", version, len(recreated))
	default:
		return fmt.Sprintf("Release r%d is live; recreated %s and kept %d unchanged", version, strings.Join(recreated, ", "), unchanged)
	}
}

// checkHostnames refuses a release whose public hostnames are already routed
// elsewhere, before anything is built. The routes table would refuse it anyway,
// but only at the route switch, after every image was built and every
// container passed its health gate, and as a bare constraint error.
func (d *Deployer) checkHostnames(ctx context.Context, projectID string, services []store.Service) error {
	seen := map[string]string{}
	hostnames := []string{}
	for _, svc := range services {
		if !svc.Public || svc.Hostname == "" {
			continue
		}
		if other, ok := seen[svc.Hostname]; ok {
			return fmt.Errorf("services %s and %s both route %s; give each public service its own hostname", other, svc.Name, svc.Hostname)
		}
		seen[svc.Hostname] = svc.Name
		hostnames = append(hostnames, svc.Hostname)
	}
	owners, err := d.Store.HostnameOwners(ctx, projectID, hostnames)
	if err != nil {
		return err
	}
	for _, hostname := range hostnames {
		if owner, taken := owners[hostname]; taken {
			return fmt.Errorf("%s is already routed to project %s; choose another hostname for %s", hostname, owner, seen[hostname])
		}
	}
	return nil
}

// createMountDirectory creates the source-tree directory a project-relative
// mount needs when the project does not ship it, and does nothing when the path
// already exists — including when it exists as a file, which is the case for
// every config file a project mounts and must never be replaced by a directory.
func createMountDirectory(target string) error {
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(target, composecfg.SourceDirMode)
}

// sourceSubpath expresses a project's source directory relative to the data
// directory, which is exactly its path inside the mounted data volume.
func (d *Deployer) sourceSubpath(project store.Project) (string, error) {
	if d.DataDir == "" || project.SourcePath == "" {
		return "", nil
	}
	relative, err := filepath.Rel(d.DataDir, project.SourcePath)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", fmt.Errorf("project source %s is outside the Asgard data directory", project.SourcePath)
	}
	return filepath.ToSlash(relative), nil
}

// specHash fingerprints the container a request would create. An empty result
// only means the running container cannot be compared, so the service is
// recreated as it always was.
func (d *Deployer) specHash(ctx context.Context, request dockerx.CreateRequest) string {
	imageID, err := d.Docker.ImageID(ctx, request.Image)
	if err != nil {
		return ""
	}
	hash, err := dockerx.SpecHash(request, imageID)
	if err != nil {
		return ""
	}
	return hash
}

// unchanged returns the previous release's container for svc when this release
// would create an identical one and the running one is in good health — the
// way `docker compose up` leaves an unchanged service alone. Recreating it
// would only cost a stop, a start, and a readiness wait, and for a database
// it is a restart nobody asked for.
func (d *Deployer) unchanged(ctx context.Context, svc store.Service, spec string, prior store.Runtime) (dockerx.Container, bool) {
	if spec == "" || prior.DockerID == "" || !keepable(svc) {
		return dockerx.Container{}, false
	}
	current, err := d.Docker.Container(ctx, prior.DockerID)
	if err != nil || current.Labels[dockerx.LabelSpec] != spec {
		return dockerx.Container{}, false
	}
	if current.State != "running" || (current.Health != "" && current.Health != "healthy") {
		return dockerx.Container{}, false
	}
	return current, true
}

// keepable reports whether a service's container may be carried into a new
// release at all. A one-shot job is run again, as it always was: whatever it
// prepares, the release expects it freshly done. A service that mounts paths
// from the project source is always recreated, because a re-sync replaces the
// source tree wholesale and a running container still holds the tree that
// was there when it started.
func keepable(svc store.Service) bool {
	if dockerx.ReadinessFor(svc).Job {
		return false
	}
	for _, spec := range svc.Volumes {
		if _, isProjectMount := composecfg.ProjectMount(spec); isProjectMount {
			return false
		}
	}
	return true
}

func stopPriorBeforeCandidate(svc store.Service) bool {
	// A private service and its replacement share the stable project-network
	// alias. Keeping both live makes dependency health checks nondeterministic.
	// Only public web services can safely remain blue/green because the edge
	// route selects the active container explicitly.
	return !svc.Public || svc.Role == "worker" || svc.Role == "stateful"
}

func candidatesForDocker(items map[string]dockerx.Container, id string) (dockerx.Container, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return dockerx.Container{}, false
}

func (d *Deployer) switchRelease(ctx context.Context, project store.Project, services []store.Service, releaseID string, candidates map[string]dockerx.Container, kept map[string]bool, old map[string]store.Runtime, log func(string)) error {
	runtimeRows := map[string]struct{ DockerID, Name, ImageID, State string }{}
	for serviceID, item := range candidates {
		runtimeRows[serviceID] = struct{ DockerID, Name, ImageID, State string }{item.ID, item.Name, item.ImageID, item.State}
	}
	tx, err := d.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := store.Now()
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_containers SET active=0,updated_at=? WHERE project_id=? AND active=1`, now, project.ID); err != nil {
		return err
	}
	for serviceID, item := range runtimeRows {
		if kept[serviceID] {
			// A container carried over unchanged keeps its one runtime row, and
			// with it the release that created it — which is the release whose
			// image it runs. Only its active flag returns.
			if _, err = tx.ExecContext(ctx, `UPDATE runtime_containers SET active=1,updated_at=? WHERE docker_id=?`, now, item.DockerID); err != nil {
				return err
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_containers(id,project_id,service_id,release_id,docker_id,docker_name,image_id,state,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`, uuid.NewString(), project.ID, serviceID, releaseID, item.DockerID, item.Name, item.ImageID, item.State, now, now); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM routes WHERE project_id=?`, project.ID); err != nil {
		return err
	}
	for _, svc := range services {
		if !svc.Public {
			continue
		}
		if svc.Port <= 0 || svc.Hostname == "" {
			return fmt.Errorf("public service %s needs hostname and port", svc.Name)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO routes(id,project_id,service_id,hostname,target_port,tls,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`, uuid.NewString(), project.ID, svc.ID, svc.Hostname, svc.Port, now, now); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	rollback := func() {
		rollbackCtx := context.WithoutCancel(ctx)
		rollbackTx, txErr := d.Store.DB.BeginTx(rollbackCtx, nil)
		if txErr == nil {
			_, _ = rollbackTx.ExecContext(rollbackCtx, `UPDATE runtime_containers SET active=0 WHERE release_id=?`, releaseID)
			for _, runtime := range old {
				_, _ = rollbackTx.ExecContext(rollbackCtx, `UPDATE runtime_containers SET active=1 WHERE docker_id=?`, runtime.DockerID)
			}
			_ = rollbackTx.Commit()
			_ = d.Proxy.Write(rollbackCtx)
		}
	}
	if err = d.Proxy.Write(ctx); err != nil {
		rollback()
		return fmt.Errorf("write proxy routes: %w", err)
	}
	for _, svc := range services {
		if !svc.Public {
			continue
		}
		endpoint := "https://" + svc.Hostname + svc.HealthPath
		log("Confirming edge readiness for " + svc.Name)
		if err = d.Docker.WaitHTTPReady(ctx, endpoint, dockerx.ReadinessTimeout(svc), log); err != nil {
			rollback()
			return fmt.Errorf("confirm edge route %s: %w", svc.Name, err)
		}
	}
	return nil
}

// holdBuilds keeps storage reclamation from running during an image build.
func (d *Deployer) holdBuilds() (release func()) {
	if d.Reclaimer == nil {
		return func() {}
	}
	return d.Reclaimer.HoldBuilds()
}

// reclaim frees images left by releases that have aged past the retention
// window. It runs only after the release is fully live, and its failure is
// never the deployment's failure: a disk that could not be tidied is a smaller
// problem than a release rolled back for it.
func (d *Deployer) reclaim(ctx context.Context, log func(string)) {
	if d.Reclaimer == nil {
		return
	}
	result, err := d.Reclaimer.Run(context.WithoutCancel(ctx))
	if err != nil {
		log("Storage reclamation skipped: " + err.Error())
		return
	}
	if result.ImagesRemoved == 0 && result.BuildCacheBytes == 0 {
		return
	}
	log("Storage: " + result.Summary())
}

func releaseToServices(items []store.ReleaseService) []store.Service {
	out := make([]store.Service, 0, len(items))
	for _, item := range items {
		out = append(out, store.Service{ID: item.ServiceID, Name: item.Name, Role: item.Role, Image: item.ImageRef, Command: item.Command, Environment: item.Environment, Container: item.Container, Public: item.Public, Port: item.Port, Hostname: item.Hostname, HealthPath: item.HealthPath, CPULimit: item.CPULimit, MemoryLimit: item.MemoryLimit, PIDsLimit: item.PIDsLimit, RestartPolicy: item.RestartPolicy, DependsOn: item.DependsOn, Volumes: item.Volumes})
	}
	return out
}

func topological(services []store.Service) ([]store.Service, error) {
	byName := map[string]store.Service{}
	for _, svc := range services {
		byName[svc.Name] = svc
	}
	visited := map[string]int{}
	out := []store.Service{}
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] == 1 {
			return fmt.Errorf("dependency cycle at %s", name)
		}
		if visited[name] == 2 {
			return nil
		}
		svc, ok := byName[name]
		if !ok {
			return fmt.Errorf("unknown dependency %s", name)
		}
		visited[name] = 1
		deps := append([]string(nil), svc.DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[name] = 2
		out = append(out, svc)
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func errorText(err error) string {
	if err == nil {
		return "deployment did not complete"
	}
	return err.Error()
}
func IsNoRelease(err error) bool { return errors.Is(err, sql.ErrNoRows) }
