package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/projectsource"
	"github.com/rousoftware/asgard/internal/store"
)

// ErrNotGitSource reports a re-sync aimed at a project whose source did not
// come from a repository, and so has nothing to re-fetch.
var ErrNotGitSource = errors.New("only projects imported from Git can be re-synced")

// ResyncResult reports what a completed re-sync fetched and what it kept.
type ResyncResult struct {
	Project         store.Project               `json:"project"`
	Validation      composecfg.ValidationResult `json:"validation"`
	Commit          string                      `json:"commit"`
	Ref             string                      `json:"ref"`
	Changed         bool                        `json:"changed"`
	PreservedDotEnv bool                        `json:"preservedDotEnv"`
}

// IsGitSource reports whether a project's source type came from a repository.
func IsGitSource(sourceType string) bool {
	return sourceType == "git" || sourceType == "git-private"
}

// Resync re-clones a Git-imported project and swaps the fresh working tree in.
// An import captures the repository exactly once, so without this every later
// deployment rebuilds the commit that was current when the project was created
// and pushing to the branch has no effect on what runs.
//
// The live tree is only replaced once the incoming Compose file has been
// validated and its service reconcile has been staged, so a repository that has
// drifted out of the safe subset leaves the running project untouched.
func (i *Importer) Resync(ctx context.Context, projectID string) (ResyncResult, error) {
	project, err := i.Store.GetProject(ctx, projectID)
	if err != nil {
		return ResyncResult{}, err
	}
	if !IsGitSource(project.SourceType) || project.SourceURL == "" {
		return ResyncResult{}, ErrNotGitSource
	}
	auth, err := i.resolveAuth(ctx, project.SourceCredentialID)
	if err != nil {
		return ResyncResult{}, err
	}
	source, err := composecfg.ValidateGitSource(project.SourceURL, auth != nil && auth.credential.Kind == store.GitCredentialSSH)
	if err != nil {
		return ResyncResult{}, err
	}

	// Stage beside the live tree so the swap is a rename within one filesystem.
	staging := filepath.Join(filepath.Dir(project.SourcePath), "source.incoming")
	retired := filepath.Join(filepath.Dir(project.SourcePath), "source.retired")
	for _, path := range []string{staging, retired} {
		if err := os.RemoveAll(path); err != nil {
			return ResyncResult{}, err
		}
	}
	swapped, committed := false, false
	defer func() {
		if !committed {
			if swapped {
				_ = os.RemoveAll(project.SourcePath)
				_ = os.Rename(retired, project.SourcePath)
			}
			_ = os.RemoveAll(staging)
			return
		}
		_ = os.RemoveAll(retired)
	}()

	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := i.clone(cloneCtx, source, project.SourceRef, staging, auth); err != nil {
		return ResyncResult{}, err
	}
	commit, err := i.headCommit(cloneCtx, staging)
	if err != nil {
		return ResyncResult{}, err
	}
	// The clone's git metadata can hold credential-bearing remotes and is never
	// needed again; the deployer builds from the extracted working tree.
	if err := os.RemoveAll(filepath.Join(staging, ".git")); err != nil {
		return ResyncResult{}, err
	}

	// Asgard owns the project's .env: operators enter deployment secrets there
	// through the source editor and repositories almost never commit one.
	// Letting the clone decide would silently empty every interpolated value.
	preserved, err := carryDotEnv(project, staging)
	if err != nil {
		return ResyncResult{}, err
	}

	incoming, err := os.ReadFile(filepath.Join(staging, project.ComposePath))
	if err != nil {
		return ResyncResult{}, fmt.Errorf("read %s from the re-synced source: %w", project.ComposePath, err)
	}
	current, err := os.ReadFile(filepath.Join(project.SourcePath, project.ComposePath))
	if err != nil {
		return ResyncResult{}, fmt.Errorf("read the current %s: %w", project.ComposePath, err)
	}

	swap := func() error {
		if err := os.Rename(project.SourcePath, retired); err != nil {
			return err
		}
		if err := os.Rename(staging, project.SourcePath); err != nil {
			_ = os.Rename(retired, project.SourcePath)
			return err
		}
		swapped = true
		return nil
	}
	validation, err := projectsource.ReplaceTree(ctx, i.Store, project, i.Domain, string(current), string(incoming), staging, swap)
	if err != nil {
		return ResyncResult{Validation: validation}, err
	}
	committed = true

	if _, err := i.Store.DB.ExecContext(ctx, `UPDATE projects SET source_commit=?,updated_at=? WHERE id=?`, commit, store.Now(), project.ID); err != nil {
		return ResyncResult{}, err
	}
	updated, err := i.Store.GetProject(ctx, project.ID)
	if err != nil {
		return ResyncResult{}, err
	}
	ref := project.SourceRef
	if ref == "" {
		ref = "default branch"
	}
	return ResyncResult{
		Project:    updated,
		Validation: validation,
		Commit:     commit,
		Ref:        ref,
		// A project imported before commits were recorded has nothing to compare
		// against, so the honest answer there is that the tree may have moved.
		Changed:         project.SourceCommit == "" || project.SourceCommit != commit,
		PreservedDotEnv: preserved,
	}, nil
}

// carryDotEnv copies the live project's .env files into the incoming tree.
func carryDotEnv(project store.Project, staging string) (bool, error) {
	carried := false
	seen := map[string]bool{}
	for _, relative := range dotEnvPaths(project.ComposePath) {
		if seen[relative] {
			continue
		}
		seen[relative] = true
		data, err := os.ReadFile(filepath.Join(project.SourcePath, relative))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return carried, err
		}
		target := filepath.Join(staging, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return carried, err
		}
		if err := os.WriteFile(target, data, 0o640); err != nil {
			return carried, err
		}
		carried = true
	}
	return carried, nil
}

// dotEnvPaths lists the .env locations Asgard reads: the one beside the Compose
// file that the source editor exposes, and the project root that Compose
// interpolation consults.
func dotEnvPaths(composePath string) []string {
	return []string{
		filepath.Clean(filepath.Join(filepath.Dir(filepath.Clean(composePath)), ".env")),
		".env",
	}
}
