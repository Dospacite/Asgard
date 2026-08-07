package projectsource

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

const (
	MaxDockerfileBytes = 1 << 20
	MaxDotEnvBytes     = 256 << 10
	maxSourceFiles     = 64
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type File struct {
	Path       string                       `json:"path"`
	Kind       string                       `json:"kind"`
	Label      string                       `json:"label"`
	Content    string                       `json:"content"`
	Revision   string                       `json:"revision"`
	Exists     bool                         `json:"exists"`
	SizeBytes  int64                        `json:"sizeBytes"`
	Validation *composecfg.ValidationResult `json:"validation,omitempty"`
}

type Workspace struct {
	Files        []File                       `json:"files"`
	DotEnv       map[string]string            `json:"dotenv"`
	DotEnvErrors []composecfg.ValidationError `json:"dotenvErrors"`
}

type Problem struct {
	Code       string
	Message    string
	Validation *composecfg.ValidationResult
}

func (p *Problem) Error() string { return p.Message }

type fileSpec struct {
	Path  string
	Kind  string
	Label string
	Max   int64
}

func Load(project store.Project) (Workspace, error) {
	specs, err := allowedFiles(project)
	if err != nil {
		return Workspace{}, err
	}
	workspace := Workspace{Files: make([]File, 0, len(specs)), DotEnv: map[string]string{}, DotEnvErrors: []composecfg.ValidationError{}}
	for _, spec := range specs {
		item, err := readFile(project.SourcePath, spec)
		if err != nil {
			return Workspace{}, err
		}
		if spec.Kind == "compose" {
			_, validation := composecfg.Parse([]byte(item.Content), project.ID, project.Slug, project.SourcePath)
			item.Validation = &validation
		}
		if spec.Kind == "dotenv" && item.Exists {
			values, issues := ParseDotEnv(item.Content)
			workspace.DotEnv = values
			workspace.DotEnvErrors = issues
		}
		workspace.Files = append(workspace.Files, item)
	}
	return workspace, nil
}

func Save(ctx context.Context, database *store.Store, project store.Project, domain, path, content, expectedRevision string) (Workspace, error) {
	specs, err := allowedFiles(project)
	if err != nil {
		return Workspace{}, err
	}
	var spec fileSpec
	found := false
	for _, candidate := range specs {
		if candidate.Path == filepath.Clean(path) {
			spec, found = candidate, true
			break
		}
	}
	if !found {
		return Workspace{}, &Problem{Code: "source_file_not_allowed", Message: "Only the configured Compose file, project Dockerfiles, and project .env file can be edited."}
	}
	current, err := readFile(project.SourcePath, spec)
	if err != nil {
		return Workspace{}, err
	}
	if expectedRevision == "" || expectedRevision != current.Revision {
		return Workspace{}, &Problem{Code: "source_revision_conflict", Message: "This file changed after it was loaded. Refresh the workspace and apply the edit again."}
	}
	if int64(len(content)) > spec.Max {
		return Workspace{}, &Problem{Code: "source_file_too_large", Message: fmt.Sprintf("%s is limited to %d bytes.", spec.Label, spec.Max)}
	}

	var incoming, previous composecfg.ValidationResult
	if spec.Kind == "compose" {
		_, incoming = composecfg.Parse([]byte(content), project.ID, project.Slug, project.SourcePath)
		adaptDomain(&incoming, domain)
		if !incoming.Valid {
			return Workspace{}, &Problem{Code: "compose_invalid", Message: "Compose validation failed. Fix the listed issues before saving.", Validation: &incoming}
		}
		_, previous = composecfg.Parse([]byte(current.Content), project.ID, project.Slug, project.SourcePath)
		adaptDomain(&previous, domain)
		if err := rejectServiceRemoval(project.Services, incoming.Services, &incoming); err != nil {
			return Workspace{}, err
		}
	}
	if spec.Kind == "dotenv" {
		_, issues := ParseDotEnv(content)
		if len(issues) > 0 {
			validation := composecfg.ValidationResult{Valid: false, Errors: issues, Warnings: []composecfg.ValidationError{}, Services: []store.Service{}}
			return Workspace{}, &Problem{Code: "dotenv_invalid", Message: "The .env file contains invalid entries. Fix the listed issues before saving.", Validation: &validation}
		}
	}

	if spec.Kind == "compose" {
		err = saveCompose(ctx, database, project, spec, current, content, previous, incoming)
	} else {
		err = writeAtomic(project.SourcePath, spec, current, content, func() error {
			_, updateErr := database.DB.ExecContext(ctx, `UPDATE projects SET updated_at=? WHERE id=?`, store.Now(), project.ID)
			return updateErr
		})
	}
	if err != nil {
		return Workspace{}, err
	}
	updated, err := database.GetProject(ctx, project.ID)
	if err != nil {
		return Workspace{}, err
	}
	return Load(updated)
}

func ParseDotEnv(content string) (map[string]string, []composecfg.ValidationError) {
	values := map[string]string{}
	issues := []composecfg.ValidationError{}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 4096), MaxDotEnvBytes)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		path := fmt.Sprintf(".env.%d", lineNumber)
		if !ok || !environmentNamePattern.MatchString(key) {
			issues = append(issues, composecfg.ValidationError{Path: path, Message: "Use NAME=value with a variable name containing letters, numbers, and underscores."})
			continue
		}
		value, err := parseDotEnvValue(strings.TrimSpace(raw))
		if err != nil {
			issues = append(issues, composecfg.ValidationError{Path: path, Message: err.Error()})
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		issues = append(issues, composecfg.ValidationError{Path: ".env", Message: err.Error()})
	}
	return values, issues
}

func parseDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '\'' {
		end := strings.Index(raw[1:], "'")
		if end < 0 {
			return "", errors.New("Single-quoted value is missing its closing quote.")
		}
		end++
		if trailing := strings.TrimSpace(raw[end+1:]); trailing != "" && !strings.HasPrefix(trailing, "#") {
			return "", errors.New("Unexpected text after the quoted value.")
		}
		return raw[1:end], nil
	}
	if raw[0] == '"' {
		end := closingDoubleQuote(raw)
		if end < 0 {
			return "", errors.New("Double-quoted value is missing its closing quote.")
		}
		if trailing := strings.TrimSpace(raw[end+1:]); trailing != "" && !strings.HasPrefix(trailing, "#") {
			return "", errors.New("Unexpected text after the quoted value.")
		}
		value, err := strconv.Unquote(raw[:end+1])
		if err != nil {
			return "", errors.New("Double-quoted value contains an invalid escape sequence.")
		}
		return value, nil
	}
	for index, r := range raw {
		if r == '#' && index > 0 && (raw[index-1] == ' ' || raw[index-1] == '\t') {
			return strings.TrimSpace(raw[:index]), nil
		}
	}
	return strings.TrimSpace(raw), nil
}

func closingDoubleQuote(raw string) int {
	escaped := false
	for index := 1; index < len(raw); index++ {
		if escaped {
			escaped = false
			continue
		}
		if raw[index] == '\\' {
			escaped = true
			continue
		}
		if raw[index] == '"' {
			return index
		}
	}
	return -1
}

func allowedFiles(project store.Project) ([]fileSpec, error) {
	composePath, err := cleanRelative(project.ComposePath)
	if err != nil {
		return nil, err
	}
	byPath := map[string]fileSpec{
		composePath: {Path: composePath, Kind: "compose", Label: filepath.Base(composePath), Max: composecfg.MaxComposeBytes},
	}
	dotenvPath := filepath.Clean(filepath.Join(filepath.Dir(composePath), ".env"))
	byPath[dotenvPath] = fileSpec{Path: dotenvPath, Kind: "dotenv", Label: ".env", Max: MaxDotEnvBytes}
	byPath["Dockerfile"] = fileSpec{Path: "Dockerfile", Kind: "dockerfile", Label: "Dockerfile", Max: MaxDockerfileBytes}
	for _, service := range project.Services {
		if service.BuildContext == "" {
			continue
		}
		dockerfile := service.Dockerfile
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		path, err := cleanRelative(filepath.Join(service.BuildContext, dockerfile))
		if err == nil {
			byPath[path] = fileSpec{Path: path, Kind: "dockerfile", Label: filepath.Base(path), Max: MaxDockerfileBytes}
		}
	}
	root, err := filepath.Abs(project.SourcePath)
	if err != nil {
		return nil, err
	}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || len(byPath) >= maxSourceFiles {
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := entry.Name()
		if name != "Dockerfile" && !strings.HasPrefix(name, "Dockerfile.") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			rel = filepath.Clean(rel)
			byPath[rel] = fileSpec{Path: rel, Kind: "dockerfile", Label: filepath.Base(rel), Max: MaxDockerfileBytes}
		}
		return nil
	})
	specs := make([]fileSpec, 0, len(byPath))
	for _, spec := range byPath {
		specs = append(specs, spec)
	}
	order := map[string]int{"compose": 0, "dockerfile": 1, "dotenv": 2}
	sort.Slice(specs, func(i, j int) bool {
		if order[specs[i].Kind] != order[specs[j].Kind] {
			return order[specs[i].Kind] < order[specs[j].Kind]
		}
		return specs[i].Path < specs[j].Path
	})
	return specs, nil
}

func readFile(root string, spec fileSpec) (File, error) {
	path, info, err := securePath(root, spec.Path, true)
	if err != nil {
		return File{}, err
	}
	item := File{Path: spec.Path, Kind: spec.Kind, Label: spec.Label, Exists: info != nil}
	if info == nil {
		item.Revision = revision(spec.Path, false, nil)
		return item, nil
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("source file %s is not a regular file", spec.Path)
	}
	file, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, spec.Max+1))
	if err != nil {
		return File{}, err
	}
	if int64(len(data)) > spec.Max {
		return File{}, fmt.Errorf("source file %s exceeds %d bytes", spec.Path, spec.Max)
	}
	item.Content = string(data)
	item.SizeBytes = int64(len(data))
	item.Revision = revision(spec.Path, true, data)
	return item, nil
}

func cleanRelative(path string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.ContainsRune(clean, 0) {
		return "", errors.New("source path must stay inside the project")
	}
	return clean, nil
}

func securePath(root, relative string, allowMissing bool) (string, os.FileInfo, error) {
	clean, err := cleanRelative(relative)
	if err != nil {
		return "", nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", nil, err
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) && allowMissing && index == len(parts)-1 {
			return current, nil, nil
		}
		if statErr != nil {
			return "", nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("source path %s contains a symbolic link", relative)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", nil, fmt.Errorf("source path %s has a non-directory parent", relative)
		}
		if index == len(parts)-1 {
			return current, info, nil
		}
	}
	return "", nil, errors.New("invalid source path")
}

func revision(path string, exists bool, data []byte) string {
	hash := sha256.New()
	if exists {
		hash.Write([]byte("file\x00"))
	} else {
		hash.Write([]byte("missing\x00"))
	}
	hash.Write([]byte(filepath.ToSlash(path)))
	hash.Write([]byte{0})
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func rejectServiceRemoval(current, incoming []store.Service, validation *composecfg.ValidationResult) error {
	names := map[string]bool{}
	for _, service := range incoming {
		names[service.Name] = true
	}
	removed := []string{}
	for _, service := range current {
		if !names[service.Name] {
			removed = append(removed, service.Name)
			validation.Errors = append(validation.Errors, composecfg.ValidationError{Path: "services." + service.Name, Message: "Removing an existing service in the source editor is blocked because it can orphan containers, routes, or release history."})
		}
	}
	if len(removed) == 0 {
		return nil
	}
	sort.Strings(removed)
	validation.Valid = false
	return &Problem{Code: "service_removal_blocked", Message: "Compose would remove existing service(s): " + strings.Join(removed, ", ") + ". Remove their lifecycle state through a dedicated service operation first.", Validation: validation}
}

func adaptDomain(result *composecfg.ValidationResult, domain string) {
	if domain == "" || domain == "asgard.rousoftware.com" {
		return
	}
	for index := range result.Services {
		result.Services[index].Hostname = strings.ReplaceAll(result.Services[index].Hostname, "asgard.rousoftware.com", domain)
	}
}

func saveCompose(ctx context.Context, database *store.Store, project store.Project, spec fileSpec, current File, content string, previous, incoming composecfg.ValidationResult) error {
	tx, err := database.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previousByName := map[string]store.Service{}
	if previous.Valid {
		for _, service := range previous.Services {
			previousByName[service.Name] = service
		}
	}
	currentByName := map[string]store.Service{}
	for _, service := range project.Services {
		currentByName[service.Name] = service
	}
	for _, parsed := range incoming.Services {
		currentService, exists := currentByName[parsed.Name]
		if !exists {
			parsed.ID = uuid.NewString()
			parsed.ProjectID = project.ID
			if err := insertService(ctx, tx, parsed); err != nil {
				return err
			}
			continue
		}
		merged := mergeService(currentService, previousByName[parsed.Name], parsed, previous.Valid)
		if err := updateComposedService(ctx, tx, merged, currentService.ConfigRevision); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET primary_service=?,updated_at=? WHERE id=?`, incoming.PrimaryService, store.Now(), project.ID); err != nil {
		return err
	}
	rollback, err := replaceFile(project.SourcePath, spec, current, content)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		_ = rollback.rollback()
		return err
	}
	return rollback.commit()
}

func mergeService(current, previous, incoming store.Service, previousValid bool) store.Service {
	merged := incoming
	merged.ID = current.ID
	merged.ProjectID = current.ProjectID
	merged.CPULimit = current.CPULimit
	merged.MemoryLimit = current.MemoryLimit
	merged.PIDsLimit = current.PIDsLimit
	merged.ConfigRevision = current.ConfigRevision
	if !previousValid || previous.Name == "" {
		merged.Role = current.Role
		merged.Environment = cloneMap(current.Environment)
		merged.Public = current.Public
		merged.Port = current.Port
		merged.Hostname = current.Hostname
		merged.HealthPath = current.HealthPath
		merged.RestartPolicy = current.RestartPolicy
		return merged
	}
	merged.Role = chooseString(current.Role, previous.Role, incoming.Role)
	merged.Public = chooseBool(current.Public, previous.Public, incoming.Public)
	merged.Port = chooseInt(current.Port, previous.Port, incoming.Port)
	merged.Hostname = chooseString(current.Hostname, previous.Hostname, incoming.Hostname)
	merged.HealthPath = chooseString(current.HealthPath, previous.HealthPath, incoming.HealthPath)
	merged.RestartPolicy = chooseString(current.RestartPolicy, previous.RestartPolicy, incoming.RestartPolicy)
	merged.Environment = mergeEnvironment(current.Environment, previous.Environment, incoming.Environment)
	return merged
}

func chooseString(current, previous, incoming string) string {
	if current == previous {
		return incoming
	}
	return current
}

func chooseInt(current, previous, incoming int) int {
	if current == previous {
		return incoming
	}
	return current
}

func chooseBool(current, previous, incoming bool) bool {
	if current == previous {
		return incoming
	}
	return current
}

func mergeEnvironment(current, previous, incoming map[string]string) map[string]string {
	merged := cloneMap(incoming)
	for key, value := range current {
		previousValue, managed := previous[key]
		if !managed || value != previousValue {
			merged[key] = value
		}
	}
	return merged
}

func cloneMap(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func insertService(ctx context.Context, tx *sql.Tx, service store.Service) error {
	command, _ := json.Marshal(service.Command)
	environment, _ := json.Marshal(service.Environment)
	dependsOn, _ := json.Marshal(service.DependsOn)
	volumes, _ := json.Marshal(service.Volumes)
	now := store.Now()
	_, err := tx.ExecContext(ctx, `INSERT INTO services(id,project_id,name,role,image,build_context,dockerfile,command_json,env_json,public,port,hostname,health_path,cpu_limit,memory_limit,pids_limit,restart_policy,depends_on_json,volumes_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, service.ID, service.ProjectID, service.Name, service.Role, service.Image, service.BuildContext, service.Dockerfile, string(command), string(environment), service.Public, service.Port, service.Hostname, service.HealthPath, service.CPULimit, service.MemoryLimit, service.PIDsLimit, service.RestartPolicy, string(dependsOn), string(volumes), now, now)
	if err != nil {
		return err
	}
	return syncVolumes(ctx, tx, service, now)
}

func updateComposedService(ctx context.Context, tx *sql.Tx, service store.Service, expectedRevision int) error {
	command, _ := json.Marshal(service.Command)
	environment, _ := json.Marshal(service.Environment)
	dependsOn, _ := json.Marshal(service.DependsOn)
	volumes, _ := json.Marshal(service.Volumes)
	now := store.Now()
	result, err := tx.ExecContext(ctx, `UPDATE services SET role=?,image=?,build_context=?,dockerfile=?,command_json=?,env_json=?,public=?,port=?,hostname=?,health_path=?,restart_policy=?,depends_on_json=?,volumes_json=?,config_revision=config_revision+1,updated_at=? WHERE id=? AND config_revision=?`, service.Role, service.Image, service.BuildContext, service.Dockerfile, string(command), string(environment), service.Public, service.Port, service.Hostname, service.HealthPath, service.RestartPolicy, string(dependsOn), string(volumes), now, service.ID, expectedRevision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return &Problem{Code: "service_revision_conflict", Message: "A service configuration changed while Compose was being saved. Refresh and apply the edit again."}
	}
	return syncVolumes(ctx, tx, service, now)
}

func syncVolumes(ctx context.Context, tx *sql.Tx, service store.Service, now string) error {
	for _, spec := range service.Volumes {
		parts := strings.SplitN(spec, ":", 3)
		if len(parts) < 2 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO volumes(id,project_id,service_id,name,mount_path,created_at) VALUES(?,?,?,?,?,?)`, uuid.NewString(), service.ProjectID, service.ID, parts[0], parts[1], now); err != nil {
			return err
		}
	}
	return nil
}

type fileRollback struct {
	target  string
	backup  string
	existed bool
}

func writeAtomic(root string, spec fileSpec, current File, content string, beforeCommit func() error) error {
	rollback, err := replaceFile(root, spec, current, content)
	if err != nil {
		return err
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			_ = rollback.rollback()
			return err
		}
	}
	return rollback.commit()
}

func replaceFile(root string, spec fileSpec, current File, content string) (fileRollback, error) {
	target, _, err := securePath(root, spec.Path, true)
	if err != nil {
		return fileRollback{}, err
	}
	directory := filepath.Dir(target)
	temp, err := os.CreateTemp(directory, ".asgard-source-*")
	if err != nil {
		return fileRollback{}, err
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	mode := os.FileMode(0o640)
	if current.Exists {
		if info, statErr := os.Stat(target); statErr == nil {
			mode = info.Mode().Perm()
		}
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return fileRollback{}, err
	}
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		return fileRollback{}, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fileRollback{}, err
	}
	if err := temp.Close(); err != nil {
		return fileRollback{}, err
	}
	state := fileRollback{target: target, existed: current.Exists}
	if current.Exists {
		backupFile, err := os.CreateTemp(directory, ".asgard-backup-*")
		if err != nil {
			return fileRollback{}, err
		}
		state.backup = backupFile.Name()
		backupFile.Close()
		_ = os.Remove(state.backup)
		if err := os.Rename(target, state.backup); err != nil {
			return fileRollback{}, err
		}
	}
	if err := os.Rename(tempPath, target); err != nil {
		if state.existed {
			_ = os.Rename(state.backup, target)
		}
		return fileRollback{}, err
	}
	cleanup = false
	return state, nil
}

func (state fileRollback) rollback() error {
	if state.existed {
		_ = os.Remove(state.target)
		return os.Rename(state.backup, state.target)
	}
	return os.Remove(state.target)
}

func (state fileRollback) commit() error {
	if state.backup != "" {
		return os.Remove(state.backup)
	}
	return nil
}
