package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Release struct {
	ID              string           `json:"id"`
	ProjectID       string           `json:"projectId"`
	Version         int              `json:"version"`
	Status          string           `json:"status"`
	SourceRevision  string           `json:"sourceRevision"`
	ComposeSnapshot string           `json:"-"`
	CreatedAt       time.Time        `json:"createdAt"`
	CompletedAt     *time.Time       `json:"completedAt,omitempty"`
	Services        []ReleaseService `json:"services,omitempty"`
}

type ReleaseService struct {
	ID            string            `json:"id"`
	ReleaseID     string            `json:"releaseId"`
	ServiceID     string            `json:"serviceId"`
	Name          string            `json:"name"`
	Role          string            `json:"role"`
	ImageRef      string            `json:"imageRef"`
	Command       []string          `json:"command"`
	Environment   map[string]string `json:"environment"`
	Public        bool              `json:"public"`
	Port          int               `json:"port"`
	Hostname      string            `json:"hostname"`
	HealthPath    string            `json:"healthPath"`
	CPULimit      float64           `json:"cpuLimit"`
	MemoryLimit   int64             `json:"memoryLimit"`
	PIDsLimit     int64             `json:"pidsLimit"`
	RestartPolicy string            `json:"restartPolicy"`
	DependsOn     []string          `json:"dependsOn"`
	Volumes       []string          `json:"volumes"`
}

type Deployment struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId"`
	ReleaseID   string     `json:"releaseId"`
	OperationID string     `json:"operationId"`
	Status      string     `json:"status"`
	TriggerType string     `json:"triggerType"`
	Error       string     `json:"error"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

func (s *Store) BeginRelease(ctx context.Context, projectID, operationID, trigger, sourceRevision, composeSnapshot string, services []Service) (Release, Deployment, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Release{}, Deployment{}, err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM releases WHERE project_id=?`, projectID).Scan(&version); err != nil {
		return Release{}, Deployment{}, err
	}
	release := Release{ID: uuid.NewString(), ProjectID: projectID, Version: version, Status: "deploying", SourceRevision: sourceRevision, ComposeSnapshot: composeSnapshot, CreatedAt: time.Now().UTC()}
	deployment := Deployment{ID: uuid.NewString(), ProjectID: projectID, ReleaseID: release.ID, OperationID: operationID, Status: "running", TriggerType: trigger, StartedAt: time.Now().UTC()}
	now := Now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO releases(id,project_id,version,status,source_revision,compose_snapshot,created_at) VALUES(?,?,?,?,?,?,?)`, release.ID, projectID, version, release.Status, sourceRevision, composeSnapshot, now); err != nil {
		return Release{}, Deployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployments(id,project_id,release_id,operation_id,status,trigger_type,started_at) VALUES(?,?,?,?,?,?,?)`, deployment.ID, projectID, release.ID, operationID, deployment.Status, trigger, now); err != nil {
		return Release{}, Deployment{}, err
	}
	for _, svc := range services {
		cmd, _ := json.Marshal(svc.Command)
		env, _ := json.Marshal(svc.Environment)
		deps, _ := json.Marshal(svc.DependsOn)
		volumes, _ := json.Marshal(svc.Volumes)
		_, err = tx.ExecContext(ctx, `INSERT INTO release_services(id,release_id,service_id,name,role,image_ref,command_json,env_json,public,port,hostname,health_path,cpu_limit,memory_limit,pids_limit,restart_policy,depends_on_json,volumes_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, uuid.NewString(), release.ID, svc.ID, svc.Name, svc.Role, svc.Image, string(cmd), string(env), svc.Public, svc.Port, svc.Hostname, svc.HealthPath, svc.CPULimit, svc.MemoryLimit, svc.PIDsLimit, svc.RestartPolicy, string(deps), string(volumes))
		if err != nil {
			return Release{}, Deployment{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Release{}, Deployment{}, err
	}
	return release, deployment, nil
}

func (s *Store) SetReleaseServiceImage(ctx context.Context, releaseID, serviceID, imageRef string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE release_services SET image_ref=? WHERE release_id=? AND service_id=?`, imageRef, releaseID, serviceID)
	return err
}

func (s *Store) FinishRelease(ctx context.Context, releaseID, deploymentID, status, errorMessage string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	if _, err = tx.ExecContext(ctx, `UPDATE releases SET status=?,completed_at=? WHERE id=?`, status, now, releaseID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET status=?,error=?,finished_at=? WHERE id=?`, status, errorMessage, now, deploymentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ActivateRuntime(ctx context.Context, releaseID string, containers map[string]struct{ DockerID, Name, ImageID, State string }) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID string
	if err = tx.QueryRowContext(ctx, `SELECT project_id FROM releases WHERE id=?`, releaseID).Scan(&projectID); err != nil {
		return err
	}
	now := Now()
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_containers SET active=0,updated_at=? WHERE project_id=? AND active=1`, now, projectID); err != nil {
		return err
	}
	for serviceID, item := range containers {
		if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_containers(id,project_id,service_id,release_id,docker_id,docker_name,image_id,state,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`, uuid.NewString(), projectID, serviceID, releaseID, item.DockerID, item.Name, item.ImageID, item.State, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateRuntimeState(ctx context.Context, dockerID, state string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE runtime_containers SET state=?,updated_at=? WHERE docker_id=?`, state, Now(), dockerID)
	return err
}

func (s *Store) ActiveRuntimes(ctx context.Context, projectID string) (map[string]Runtime, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT service_id,docker_id,docker_name,image_id,state,active,created_at,updated_at FROM runtime_containers WHERE project_id=? AND active=1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Runtime{}
	for rows.Next() {
		var serviceID, created, updated string
		var rt Runtime
		var active int
		if err := rows.Scan(&serviceID, &rt.DockerID, &rt.DockerName, &rt.ImageID, &rt.State, &active, &created, &updated); err != nil {
			return nil, err
		}
		rt.Active = active != 0
		rt.CreatedAt = parseTime(created)
		rt.UpdatedAt = parseTime(updated)
		out[serviceID] = rt
	}
	return out, rows.Err()
}

func (s *Store) ListDeployments(ctx context.Context, projectID string, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id,project_id,release_id,operation_id,status,trigger_type,error,started_at,finished_at FROM deployments`
	args := []any{}
	if projectID != "" {
		query += ` WHERE project_id=?`
		args = append(args, projectID)
	}
	query += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		var started string
		var finished sql.NullString
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.ReleaseID, &d.OperationID, &d.Status, &d.TriggerType, &d.Error, &started, &finished); err != nil {
			return nil, err
		}
		d.StartedAt = parseTime(started)
		d.FinishedAt = parseTimePtr(finished)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetRelease(ctx context.Context, id string) (Release, error) {
	var release Release
	var created string
	var completed sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id,project_id,version,status,source_revision,compose_snapshot,created_at,completed_at FROM releases WHERE id=?`, id).Scan(&release.ID, &release.ProjectID, &release.Version, &release.Status, &release.SourceRevision, &release.ComposeSnapshot, &created, &completed)
	if err != nil {
		return release, err
	}
	release.CreatedAt = parseTime(created)
	release.CompletedAt = parseTimePtr(completed)
	release.Services, err = s.ReleaseServices(ctx, id)
	return release, err
}

func (s *Store) ReleaseServices(ctx context.Context, releaseID string) ([]ReleaseService, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,release_id,service_id,name,role,image_ref,command_json,env_json,public,port,hostname,health_path,cpu_limit,memory_limit,pids_limit,restart_policy,depends_on_json,volumes_json FROM release_services WHERE release_id=? ORDER BY name`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReleaseService{}
	for rows.Next() {
		var item ReleaseService
		var command, environment, depends, volumes string
		var public int
		if err := rows.Scan(&item.ID, &item.ReleaseID, &item.ServiceID, &item.Name, &item.Role, &item.ImageRef, &command, &environment, &public, &item.Port, &item.Hostname, &item.HealthPath, &item.CPULimit, &item.MemoryLimit, &item.PIDsLimit, &item.RestartPolicy, &depends, &volumes); err != nil {
			return nil, err
		}
		item.Public = public != 0
		_ = json.Unmarshal([]byte(command), &item.Command)
		_ = json.Unmarshal([]byte(environment), &item.Environment)
		_ = json.Unmarshal([]byte(depends), &item.DependsOn)
		_ = json.Unmarshal([]byte(volumes), &item.Volumes)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) PreviousSuccessfulRelease(ctx context.Context, projectID, exclude string) (Release, error) {
	var id string
	query := `SELECT id FROM releases WHERE project_id=? AND status='succeeded'`
	args := []any{projectID}
	if exclude != "" {
		query += ` AND id<>?`
		args = append(args, exclude)
	}
	query += ` ORDER BY version DESC LIMIT 1`
	if err := s.DB.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return Release{}, err
	}
	return s.GetRelease(ctx, id)
}

func (s *Store) LatestSuccessfulRelease(ctx context.Context, projectID string) (Release, error) {
	return s.PreviousSuccessfulRelease(ctx, projectID, "")
}

func (s *Store) TrimReleases(ctx context.Context, projectID string, keep int) error {
	if keep < 1 {
		return errors.New("keep must be positive")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM releases WHERE project_id=? AND status='succeeded' ORDER BY version DESC LIMIT -1 OFFSET ?`, projectID, keep)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.DB.ExecContext(ctx, `DELETE FROM releases WHERE id=? AND id NOT IN (SELECT release_id FROM runtime_containers WHERE active=1)`, id); err != nil {
			return fmt.Errorf("trim release: %w", err)
		}
	}
	return nil
}
