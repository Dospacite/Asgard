package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/secrets"
	"github.com/rousoftware/asgard/internal/store"
)

type Importer struct {
	Store       *store.Store
	ProjectsDir string
	DataDir     string
	Domain      string
	Secrets     *secrets.Box
}

type Request struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Description  string `json:"description"`
	URL          string `json:"url"`
	Ref          string `json:"ref"`
	CredentialID string `json:"credentialId"`
	Image        string `json:"image"`
	Port         int    `json:"port"`
	Public       bool   `json:"public"`
	ComposePath  string `json:"composePath"`
}

// FromGit clones a repository, optionally authenticating with a stored
// credential so private repositories can be imported.
func (i *Importer) FromGit(ctx context.Context, req Request) (store.Project, composecfg.ValidationResult, error) {
	auth, err := i.resolveAuth(ctx, req.CredentialID)
	if err != nil {
		return store.Project{}, composecfg.ValidationResult{}, err
	}
	source, err := composecfg.ValidateGitSource(req.URL, auth != nil && auth.credential.Kind == store.GitCredentialSSH)
	if err != nil {
		return store.Project{}, composecfg.ValidationResult{}, err
	}
	sourceType := "git"
	if auth != nil {
		sourceType = "git-private"
	}
	p, root, err := i.prepare(req, sourceType)
	if err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()
	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := i.clone(cloneCtx, source, req.Ref, root, auth); err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	commit, err := i.headCommit(cloneCtx, root)
	if err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	// The clone's git metadata can hold credential-bearing remotes and is never
	// needed again; the deployer builds from the extracted working tree.
	_ = os.RemoveAll(filepath.Join(root, ".git"))
	p.SourceURL = source.URL
	p.SourceRef = req.Ref
	p.SourceCommit = commit
	if auth != nil {
		p.SourceCredentialID = auth.credential.ID
	}
	result, err := i.finish(ctx, &p, root, req.ComposePath)
	if err == nil {
		ok = true
		if auth != nil {
			_ = i.Store.TouchGitCredential(ctx, auth.credential.ID)
		}
	}
	return p, result, err
}

// FromArchive imports a project from any supported upload container. The
// original filename is only a hint; the archive's own bytes decide the format.
func (i *Importer) FromArchive(ctx context.Context, req Request, archivePath, filename string) (store.Project, composecfg.ValidationResult, error) {
	format, err := DetectFormat(archivePath, filename)
	if err != nil {
		return store.Project{}, composecfg.ValidationResult{}, err
	}
	p, root, err := i.prepare(req, string(format))
	if err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()
	if err := ExtractArchive(archivePath, root, filename); err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	if err := flattenSingleRoot(root); err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	result, err := i.finish(ctx, &p, root, req.ComposePath)
	if err == nil {
		ok = true
	}
	return p, result, err
}

func (i *Importer) FromZIP(ctx context.Context, req Request, zipPath string) (store.Project, composecfg.ValidationResult, error) {
	return i.FromArchive(ctx, req, zipPath, "upload.zip")
}

func (i *Importer) FromImage(ctx context.Context, req Request) (store.Project, composecfg.ValidationResult, error) {
	if err := composecfg.ValidateImageReference(req.Image); err != nil {
		return store.Project{}, composecfg.ValidationResult{}, err
	}
	if req.Port < 0 || req.Port > 65535 {
		return store.Project{}, composecfg.ValidationResult{}, errors.New("port must be between 1 and 65535")
	}
	p, root, err := i.prepare(req, "image")
	if err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()
	public := req.Public && req.Port > 0
	compose := fmt.Sprintf("services:\n  app:\n    image: %s\n", quoteYAML(req.Image))
	if req.Port > 0 {
		compose += fmt.Sprintf("    expose:\n      - %d\n", req.Port)
	}
	compose += fmt.Sprintf("x-asgard:\n  primary-service: app\n  services:\n    app:\n      role: web\n      public: %t\n      port: %d\n      health-path: /\n", public, req.Port)
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte(compose), 0o640); err != nil {
		return p, composecfg.ValidationResult{}, err
	}
	p.SourceURL = req.Image
	result, err := i.finish(ctx, &p, root, "compose.yaml")
	if err == nil {
		ok = true
	}
	return p, result, err
}

func (i *Importer) prepare(req Request, sourceType string) (store.Project, string, error) {
	slug := composecfg.Slug(req.Slug)
	if slug == "" {
		slug = composecfg.Slug(req.Name)
	}
	if !composecfg.ValidateSlug(slug) {
		return store.Project{}, "", errors.New("project slug must be a valid DNS label")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = slug
	}
	if len(name) > 100 {
		return store.Project{}, "", errors.New("project name is too long")
	}
	id := uuid.NewString()
	root := filepath.Join(i.ProjectsDir, id, "source")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return store.Project{}, "", err
	}
	return store.Project{ID: id, Slug: slug, Name: name, Description: strings.TrimSpace(req.Description), SourceType: sourceType, SourcePath: root}, root, nil
}

func (i *Importer) finish(ctx context.Context, p *store.Project, root, composePath string) (composecfg.ValidationResult, error) {
	if composePath == "" {
		found, err := FindCompose(root)
		if err != nil {
			return composecfg.ValidationResult{}, err
		}
		composePath = found
	}
	clean := filepath.Clean(composePath)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return composecfg.ValidationResult{}, errors.New("compose path escapes project root")
	}
	data, err := os.ReadFile(filepath.Join(root, clean))
	if err != nil {
		return composecfg.ValidationResult{}, fmt.Errorf("read Compose file: %w", err)
	}
	_, result := composecfg.Parse(data, p.ID, p.Slug, root)
	if !result.Valid {
		return result, validationError(result.Errors)
	}
	p.ComposePath = clean
	p.PrimaryService = result.PrimaryService
	for index := range result.Services {
		result.Services[index].Hostname = strings.ReplaceAll(result.Services[index].Hostname, "asgard.rousoftware.com", i.Domain)
	}
	if err := i.Store.CreateProject(ctx, *p, result.Services); err != nil {
		return result, err
	}
	created, err := i.Store.GetProject(ctx, p.ID)
	if err == nil {
		*p = created
	}
	return result, err
}

func validationError(items []composecfg.ValidationError) error {
	if len(items) == 0 {
		return errors.New("Compose validation failed")
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.Path+": "+item.Message)
	}
	return fmt.Errorf("Compose validation failed: %s", strings.Join(parts, "; "))
}

func SaveUpload(dst string, src io.Reader, max int64) error {
	file, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := io.CopyN(file, src, max+1)
	if err != nil && err != io.EOF {
		return err
	}
	if written > max {
		return errors.New("upload exceeds size limit")
	}
	return nil
}

var credentialPattern = regexp.MustCompile(`(?i)(token|password|authorization|secret)=([^&\s]+)`)

func sanitizeOutput(value []byte) string {
	text := credentialPattern.ReplaceAllString(string(value), "$1=[redacted]")
	if len(text) > 2048 {
		text = text[len(text)-2048:]
	}
	return strings.TrimSpace(text)
}
func quoteYAML(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
