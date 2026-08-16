package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.Migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, schema); err != nil {
		return err
	}
	for _, item := range addedColumns {
		exists, err := s.columnExists(ctx, item.table, item.column)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", item.table, item.column, item.definition)); err != nil {
			return fmt.Errorf("add %s.%s: %w", item.table, item.column, err)
		}
	}
	return nil
}

func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var name, columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&index, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// IsNamedVolumeSpec reports whether a stored volume spec refers to a Docker
// named volume. Project-relative mounts are served from Asgard's data volume
// and must never be registered as backup-eligible named volumes of their own.
func IsNamedVolumeSpec(spec string) bool {
	source, _, found := strings.Cut(spec, ":")
	return found && source != "" && !strings.HasPrefix(source, "@") && !strings.HasPrefix(source, "/") && !strings.HasPrefix(source, ".")
}

type Project struct {
	ID                 string    `json:"id"`
	Slug               string    `json:"slug"`
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	SourceType         string    `json:"sourceType"`
	SourceURL          string    `json:"sourceUrl"`
	SourceRef          string    `json:"sourceRef"`
	SourceCredentialID string    `json:"sourceCredentialId,omitempty"`
	SourcePath         string    `json:"-"`
	ComposePath        string    `json:"composePath"`
	PrimaryService     string    `json:"primaryService"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	Services           []Service `json:"services,omitempty"`
	Status             string    `json:"status"`
}

type Service struct {
	ID             string            `json:"id"`
	ProjectID      string            `json:"projectId"`
	Name           string            `json:"name"`
	Role           string            `json:"role"`
	Image          string            `json:"image"`
	BuildContext   string            `json:"buildContext"`
	Dockerfile     string            `json:"dockerfile"`
	Command        []string          `json:"command"`
	Environment    map[string]string `json:"environment"`
	Public         bool              `json:"public"`
	Port           int               `json:"port"`
	Hostname       string            `json:"hostname"`
	HealthPath     string            `json:"healthPath"`
	CPULimit       float64           `json:"cpuLimit"`
	MemoryLimit    int64             `json:"memoryLimit"`
	PIDsLimit      int64             `json:"pidsLimit"`
	RestartPolicy  string            `json:"restartPolicy"`
	DependsOn      []string          `json:"dependsOn"`
	Volumes        []string          `json:"volumes"`
	Networks       []NetworkRef      `json:"networks,omitempty"`
	ConfigRevision int               `json:"configRevision"`
	CreatedAt      time.Time         `json:"createdAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
	Runtime        *Runtime          `json:"runtime,omitempty"`
	Metrics        *Metrics          `json:"metrics,omitempty"`
}

type NetworkRef struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	DockerName string `json:"dockerName"`
	Alias      string `json:"alias"`
	Internal   bool   `json:"internal"`
}

type ManagedNetwork struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	DockerName  string          `json:"dockerName"`
	Description string          `json:"description"`
	Driver      string          `json:"driver"`
	Internal    bool            `json:"internal"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	Members     []NetworkMember `json:"members"`
}

type NetworkMember struct {
	NetworkID    string    `json:"networkId"`
	ProjectID    string    `json:"projectId"`
	ProjectSlug  string    `json:"projectSlug"`
	ProjectName  string    `json:"projectName"`
	ServiceID    string    `json:"serviceId"`
	ServiceName  string    `json:"serviceName"`
	Alias        string    `json:"alias"`
	RuntimeState string    `json:"runtimeState"`
	DockerID     string    `json:"dockerId,omitempty"`
	DockerName   string    `json:"dockerName,omitempty"`
	Connected    bool      `json:"connected"`
	IPv4Address  string    `json:"ipv4Address,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Runtime struct {
	DockerID   string    `json:"dockerId"`
	DockerName string    `json:"dockerName"`
	ImageID    string    `json:"imageId"`
	State      string    `json:"state"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Metrics struct {
	CPUPercent  float64   `json:"cpuPercent"`
	MemoryBytes int64     `json:"memoryBytes"`
	MemoryLimit int64     `json:"memoryLimit"`
	NetworkRX   int64     `json:"networkRx"`
	NetworkTX   int64     `json:"networkTx"`
	BlockRead   int64     `json:"blockRead"`
	BlockWrite  int64     `json:"blockWrite"`
	PIDs        int64     `json:"pids"`
	CollectedAt time.Time `json:"collectedAt"`
}

type Operation struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	TargetType  string          `json:"targetType"`
	TargetID    string          `json:"targetId"`
	Status      string          `json:"status"`
	Progress    int             `json:"progress"`
	Summary     string          `json:"summary"`
	Error       string          `json:"error"`
	RequestedBy string          `json:"requestedBy"`
	Payload     json.RawMessage `json:"-"`
	CreatedAt   time.Time       `json:"createdAt"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
}

func parseTime(raw string) time.Time { value, _ := time.Parse(time.RFC3339Nano, raw); return value }
func parseTimePtr(raw sql.NullString) *time.Time {
	if !raw.Valid {
		return nil
	}
	v := parseTime(raw.String)
	return &v
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,slug,name,description,source_type,source_url,source_ref,source_credential_id,source_path,compose_path,primary_service,created_at,updated_at FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	projects := []Project{}
	for rows.Next() {
		var p Project
		var created, updated string
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.SourceType, &p.SourceURL, &p.SourceRef, &p.SourceCredentialID, &p.SourcePath, &p.ComposePath, &p.PrimaryService, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range projects {
		projects[index].Services, err = s.ListServices(ctx, projects[index].ID)
		if err != nil {
			return nil, err
		}
		projects[index].Status = projectStatus(projects[index].Services)
	}
	return projects, nil
}

func (s *Store) GetProject(ctx context.Context, idOrSlug string) (Project, error) {
	var p Project
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `SELECT id,slug,name,description,source_type,source_url,source_ref,source_credential_id,source_path,compose_path,primary_service,created_at,updated_at FROM projects WHERE id=? OR slug=?`, idOrSlug, idOrSlug).
		Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.SourceType, &p.SourceURL, &p.SourceRef, &p.SourceCredentialID, &p.SourcePath, &p.ComposePath, &p.PrimaryService, &created, &updated)
	if err != nil {
		return p, err
	}
	p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
	p.Services, err = s.ListServices(ctx, p.ID)
	p.Status = projectStatus(p.Services)
	return p, err
}

func projectStatus(services []Service) string {
	if len(services) == 0 {
		return "draft"
	}
	running := 0
	for _, svc := range services {
		if svc.Runtime != nil && svc.Runtime.State == "running" {
			running++
		}
	}
	if running == len(services) {
		return "healthy"
	}
	if running > 0 {
		return "degraded"
	}
	return "stopped"
}

func (s *Store) ListServices(ctx context.Context, projectID string) ([]Service, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,project_id,name,role,image,build_context,dockerfile,command_json,env_json,public,port,hostname,health_path,cpu_limit,memory_limit,pids_limit,restart_policy,depends_on_json,volumes_json,config_revision,created_at,updated_at FROM services WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	services := []Service{}
	for rows.Next() {
		var svc Service
		var cmdJSON, envJSON, depsJSON, volumesJSON, created, updated string
		var public int
		if err := rows.Scan(&svc.ID, &svc.ProjectID, &svc.Name, &svc.Role, &svc.Image, &svc.BuildContext, &svc.Dockerfile, &cmdJSON, &envJSON, &public, &svc.Port, &svc.Hostname, &svc.HealthPath, &svc.CPULimit, &svc.MemoryLimit, &svc.PIDsLimit, &svc.RestartPolicy, &depsJSON, &volumesJSON, &svc.ConfigRevision, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		svc.Public = public != 0
		svc.CreatedAt, svc.UpdatedAt = parseTime(created), parseTime(updated)
		_ = json.Unmarshal([]byte(cmdJSON), &svc.Command)
		_ = json.Unmarshal([]byte(envJSON), &svc.Environment)
		_ = json.Unmarshal([]byte(depsJSON), &svc.DependsOn)
		_ = json.Unmarshal([]byte(volumesJSON), &svc.Volumes)
		services = append(services, svc)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range services {
		svc := &services[index]
		var rt Runtime
		var active int
		var rtCreated, rtUpdated string
		err = s.DB.QueryRowContext(ctx, `SELECT docker_id,docker_name,image_id,state,active,created_at,updated_at FROM runtime_containers WHERE service_id=? AND active=1 ORDER BY updated_at DESC LIMIT 1`, svc.ID).Scan(&rt.DockerID, &rt.DockerName, &rt.ImageID, &rt.State, &active, &rtCreated, &rtUpdated)
		if err == nil {
			rt.Active = true
			rt.CreatedAt = parseTime(rtCreated)
			rt.UpdatedAt = parseTime(rtUpdated)
			svc.Runtime = &rt
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var m Metrics
		var collected string
		err = s.DB.QueryRowContext(ctx, `SELECT cpu_percent,memory_bytes,memory_limit,network_rx,network_tx,block_read,block_write,pids,collected_at FROM metrics WHERE service_id=? ORDER BY collected_at DESC LIMIT 1`, svc.ID).Scan(&m.CPUPercent, &m.MemoryBytes, &m.MemoryLimit, &m.NetworkRX, &m.NetworkTX, &m.BlockRead, &m.BlockWrite, &m.PIDs, &collected)
		if err == nil {
			m.CollectedAt = parseTime(collected)
			svc.Metrics = &m
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		svc.Networks, err = s.ListServiceNetworks(ctx, svc.ID)
		if err != nil {
			return nil, err
		}
	}
	return services, nil
}

func (s *Store) GetService(ctx context.Context, id string) (Service, error) {
	var projectID string
	if err := s.DB.QueryRowContext(ctx, `SELECT project_id FROM services WHERE id=?`, id).Scan(&projectID); err != nil {
		return Service{}, err
	}
	services, err := s.ListServices(ctx, projectID)
	if err != nil {
		return Service{}, err
	}
	for _, svc := range services {
		if svc.ID == id {
			return svc, nil
		}
	}
	return Service{}, sql.ErrNoRows
}

func (s *Store) CreateProject(ctx context.Context, p Project, services []Service) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO projects(id,slug,name,description,source_type,source_url,source_ref,source_credential_id,source_path,compose_path,primary_service,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.Slug, p.Name, p.Description, p.SourceType, p.SourceURL, p.SourceRef, p.SourceCredentialID, p.SourcePath, p.ComposePath, p.PrimaryService, now, now)
	if err != nil {
		return err
	}
	for _, svc := range services {
		cmd, _ := json.Marshal(svc.Command)
		env, _ := json.Marshal(svc.Environment)
		deps, _ := json.Marshal(svc.DependsOn)
		vols, _ := json.Marshal(svc.Volumes)
		_, err = tx.ExecContext(ctx, `INSERT INTO services(id,project_id,name,role,image,build_context,dockerfile,command_json,env_json,public,port,hostname,health_path,cpu_limit,memory_limit,pids_limit,restart_policy,depends_on_json,volumes_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, svc.ID, p.ID, svc.Name, svc.Role, svc.Image, svc.BuildContext, svc.Dockerfile, string(cmd), string(env), svc.Public, svc.Port, svc.Hostname, svc.HealthPath, svc.CPULimit, svc.MemoryLimit, svc.PIDsLimit, svc.RestartPolicy, string(deps), string(vols), now, now)
		if err != nil {
			return err
		}
		for _, spec := range svc.Volumes {
			parts := strings.SplitN(spec, ":", 2)
			if len(parts) != 2 || !IsNamedVolumeSpec(spec) {
				continue
			}
			_, _ = tx.ExecContext(ctx, `INSERT OR IGNORE INTO volumes(id,project_id,service_id,name,mount_path,created_at) VALUES(lower(hex(randomblob(16))),?,?,?,?,?)`, p.ID, svc.ID, parts[0], parts[1], now)
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateService(ctx context.Context, svc Service, expectedRevision int) error {
	env, _ := json.Marshal(svc.Environment)
	result, err := s.DB.ExecContext(ctx, `UPDATE services SET role=?,env_json=?,public=?,port=?,hostname=?,health_path=?,cpu_limit=?,memory_limit=?,pids_limit=?,restart_policy=?,config_revision=config_revision+1,updated_at=? WHERE id=? AND config_revision=?`, svc.Role, string(env), svc.Public, svc.Port, svc.Hostname, svc.HealthPath, svc.CPULimit, svc.MemoryLimit, svc.PIDsLimit, svc.RestartPolicy, Now(), svc.ID, expectedRevision)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return errors.New("configuration changed; refresh and try again")
	}
	return nil
}

func (s *Store) AddService(ctx context.Context, svc Service) error {
	now := Now()
	cmd, _ := json.Marshal(svc.Command)
	env, _ := json.Marshal(svc.Environment)
	deps, _ := json.Marshal(svc.DependsOn)
	vols, _ := json.Marshal(svc.Volumes)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO services(id,project_id,name,role,image,build_context,dockerfile,command_json,env_json,public,port,hostname,health_path,cpu_limit,memory_limit,pids_limit,restart_policy,depends_on_json,volumes_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, svc.ID, svc.ProjectID, svc.Name, svc.Role, svc.Image, svc.BuildContext, svc.Dockerfile, string(cmd), string(env), svc.Public, svc.Port, svc.Hostname, svc.HealthPath, svc.CPULimit, svc.MemoryLimit, svc.PIDsLimit, svc.RestartPolicy, string(deps), string(vols), now, now)
	if err != nil {
		return err
	}
	for _, spec := range svc.Volumes {
		parts := strings.Split(spec, ":")
		if len(parts) >= 2 && IsNamedVolumeSpec(spec) {
			_, _ = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO volumes(id,project_id,service_id,name,mount_path,created_at) VALUES(lower(hex(randomblob(16))),?,?,?,?,?)`, svc.ProjectID, svc.ID, parts[0], parts[1], now)
		}
	}
	return nil
}

func (s *Store) CreateOperation(ctx context.Context, op Operation, idempotencyKey string) (Operation, error) {
	if len(op.Payload) == 0 {
		op.Payload = json.RawMessage(`{}`)
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO operations(id,kind,target_type,target_id,status,progress,summary,idempotency_key,requested_by,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, op.ID, op.Kind, op.TargetType, op.TargetID, "queued", 0, op.Summary, nullString(idempotencyKey), op.RequestedBy, string(op.Payload), Now())
	if err != nil && idempotencyKey != "" {
		return s.OperationByIdempotency(ctx, op.Kind, idempotencyKey)
	}
	if err != nil {
		return Operation{}, err
	}
	return s.GetOperation(ctx, op.ID)
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (s *Store) OperationByIdempotency(ctx context.Context, kind, key string) (Operation, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM operations WHERE kind=? AND idempotency_key=?`, kind, key).Scan(&id)
	if err != nil {
		return Operation{}, err
	}
	return s.GetOperation(ctx, id)
}

func (s *Store) GetOperation(ctx context.Context, id string) (Operation, error) {
	var op Operation
	var created string
	var started, completed sql.NullString
	var payload string
	err := s.DB.QueryRowContext(ctx, `SELECT id,kind,target_type,target_id,status,progress,summary,error,requested_by,payload_json,created_at,started_at,completed_at FROM operations WHERE id=?`, id).Scan(&op.ID, &op.Kind, &op.TargetType, &op.TargetID, &op.Status, &op.Progress, &op.Summary, &op.Error, &op.RequestedBy, &payload, &created, &started, &completed)
	op.Payload = json.RawMessage(payload)
	op.CreatedAt = parseTime(created)
	op.StartedAt = parseTimePtr(started)
	op.CompletedAt = parseTimePtr(completed)
	return op, err
}

func (s *Store) ListOperations(ctx context.Context, limit int) ([]Operation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM operations ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(ids))
	for _, id := range ids {
		op, err := s.GetOperation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func (s *Store) StartOperation(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE operations SET status='running',started_at=?,progress=1 WHERE id=? AND status='queued'`, Now(), id)
	return err
}
func (s *Store) ProgressOperation(ctx context.Context, id string, progress int, summary string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE operations SET progress=?,summary=? WHERE id=?`, progress, summary, id)
	return err
}
func (s *Store) CompleteOperation(ctx context.Context, id, status, errorMessage string) error {
	progress := 100
	if status != "succeeded" {
		progress = 0
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE operations SET status=?,progress=?,error=?,completed_at=? WHERE id=?`, status, progress, errorMessage, Now(), id)
	return err
}

func (s *Store) QueuedOperations(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM operations WHERE status='queued' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) LogOperation(ctx context.Context, id, level, message string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO operation_logs(operation_id,level,message,created_at) VALUES(?,?,?,?)`, id, level, message, Now())
	return err
}
func (s *Store) OperationLogs(ctx context.Context, id string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,level,message,created_at FROM operation_logs WHERE operation_id=? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var rowID int64
		var level, message, created string
		if err := rows.Scan(&rowID, &level, &message, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": rowID, "level": level, "message": message, "createdAt": created})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (s *Store) Audit(ctx context.Context, actorType, actorID, action, targetType, targetID, summary, ip, userAgent string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,target_type,target_id,summary,ip,user_agent,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, actorType, actorID, action, targetType, targetID, summary, ip, userAgent, Now())
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,actor_type,actor_id,action,target_type,target_id,summary,ip,created_at FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var actorType, actorID, action, targetType, targetID, summary, ip, created string
		if err := rows.Scan(&id, &actorType, &actorID, &action, &targetType, &targetID, &summary, &ip, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "actorType": actorType, "actorId": actorID, "action": action, "targetType": targetType, "targetId": targetID, "summary": summary, "ip": ip, "createdAt": created})
	}
	return out, rows.Err()
}

func (s *Store) Health(ctx context.Context) error {
	var one int
	return s.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
func ConflictError(entity, value string) error {
	return fmt.Errorf("%s %q already exists", entity, value)
}
