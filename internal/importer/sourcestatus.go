package importer

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

// SourceStatus compares the working tree a deployment would build against the
// current head of the tracked ref.
//
// An import captures the repository once and every later deployment rebuilds
// that captured tree, so "push, then deploy" quietly ships the old commit until
// someone remembers to re-sync. Keeping the capture explicit is the right call
// — it is what makes a release reproducible and a rollback meaningful — but the
// failure was silent, which is not. Reporting the commit being built, and
// saying so when the ref has moved ahead of it, keeps the property without the
// footgun.
type SourceStatus struct {
	// Commit is the revision the working tree holds: what a deployment starting
	// now would actually build.
	Commit string `json:"commit"`
	// RemoteCommit is the current head of the tracked ref, when it could be
	// read.
	RemoteCommit string `json:"remoteCommit,omitempty"`
	Ref          string `json:"ref,omitempty"`
	// Behind reports that the ref has moved ahead of the captured tree. A
	// deployment will still succeed; it will just build the older commit.
	Behind bool `json:"behind"`
	// Checked is false when the remote could not be consulted — no credential,
	// no network, not a Git project. The deployment is not blocked by it.
	Checked bool   `json:"checked"`
	Reason  string `json:"reason,omitempty"`
}

// Summary renders the status as the one line an operation log should carry.
func (s SourceStatus) Summary() string {
	if s.Commit == "" {
		return "Building the captured working tree; no source revision was recorded for it."
	}
	if s.Behind {
		return "Building " + shortCommit(s.Commit) + ", but " + s.refName() + " is now at " + shortCommit(s.RemoteCommit) + ". Re-sync the source first to deploy the newer commit."
	}
	if s.Checked {
		return "Building " + shortCommit(s.Commit) + ", the current head of " + s.refName() + "."
	}
	return "Building " + shortCommit(s.Commit) + ". The remote was not consulted: " + s.Reason
}

func (s SourceStatus) refName() string {
	if s.Ref == "" {
		return "the default branch"
	}
	return s.Ref
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	if commit == "" {
		return "an unknown revision"
	}
	return commit
}

// CheckSource reports which commit a deployment of this project would build and
// whether the tracked ref has moved past it.
//
// Every failure here is soft. This is advisory information attached to a
// deployment, and an unreachable remote is not a reason to refuse to deploy the
// tree that is already on disk.
func (i *Importer) CheckSource(ctx context.Context, project store.Project) SourceStatus {
	status := SourceStatus{Commit: project.SourceCommit, Ref: project.SourceRef}
	if !IsGitSource(project.SourceType) || project.SourceURL == "" {
		status.Reason = "the project was not imported from Git"
		return status
	}
	if project.SourceCommit == "" {
		status.Reason = "this project was imported before commits were recorded"
		return status
	}
	auth, err := i.resolveAuth(ctx, project.SourceCredentialID)
	if err != nil {
		status.Reason = "its credential could not be loaded"
		return status
	}
	source, err := composecfg.ValidateGitSource(project.SourceURL, auth != nil && auth.credential.Kind == store.GitCredentialSSH)
	if err != nil {
		status.Reason = err.Error()
		return status
	}
	head, err := i.remoteHead(ctx, source.URL, project.SourceRef, auth)
	if err != nil {
		status.Reason = err.Error()
		return status
	}
	status.Checked = true
	status.RemoteCommit = head
	status.Behind = head != "" && head != project.SourceCommit
	return status
}

// remoteHead reads the commit the tracked ref currently points at. An empty ref
// means the remote's default branch, which ls-remote reports through HEAD.
func (i *Importer) remoteHead(ctx context.Context, url, ref string, auth *gitAuth) (string, error) {
	environment, cleanup, err := i.gitEnvironment(auth)
	if err != nil {
		return "", err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	args := []string{"ls-remote", "--", url}
	if ref == "" {
		args = []string{"ls-remote", "--symref", "--", url, "HEAD"}
	} else {
		args = append(args, ref)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), environment...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", errTimeout
		}
		return "", errRemoteUnreadable
	}
	return firstCommit(string(output), ref), nil
}

// firstCommit picks the object id for the ref out of ls-remote's output.
// Asking for a ref by name can match both the branch and a tag of the same
// name, so the branch form is preferred and a bare match is the fallback.
func firstCommit(output, ref string) string {
	fallback := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) < 40 {
			continue
		}
		commit, name := fields[0], fields[1]
		if ref == "" && name == "HEAD" {
			return commit
		}
		if name == "refs/heads/"+ref {
			return commit
		}
		if fallback == "" {
			fallback = commit
		}
	}
	return fallback
}

var (
	errTimeout          = timeoutError("the remote did not answer within " + verifyTimeout.String())
	errRemoteUnreadable = timeoutError("the remote could not be read with this project's credential")
)

type timeoutError string

func (e timeoutError) Error() string { return string(e) }
