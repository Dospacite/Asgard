package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

// Credential verification.
//
// A stored credential is only a guess until something has used it. Asgard used
// to find out at the worst possible moment — mid-deploy, with a release
// blocked — and its own dashboard actively misled: a credential showed a recent
// lastUsedAt because it had worked for a *different* project, while being
// unable to read the repository it was attached to here.
//
// A credential is trivially testable at the moment it is stored. `git
// ls-remote` authenticates, asks the server for its refs, and transfers no
// objects, so it costs one round trip and proves the thing that matters: this
// secret can read this repository. That check runs on create, on rotate, on
// demand, and on a schedule, and its result is recorded so "broken" is visible
// days before it is blocking.

// Verification outcomes recorded against a credential.
const (
	VerifyOK       = "ok"
	VerifyFailed   = "failed"
	VerifySkipped  = "skipped"
	verifyTimeout  = 30 * time.Second
	maxVerifyError = 500
)

// VerifyResult reports one verification attempt.
type VerifyResult struct {
	CredentialID string    `json:"credentialId"`
	Status       string    `json:"status"`
	Repository   string    `json:"repository,omitempty"`
	Refs         int       `json:"refs,omitempty"`
	Error        string    `json:"error,omitempty"`
	CheckedAt    time.Time `json:"checkedAt"`
}

// OK reports whether the credential proved it works.
func (r VerifyResult) OK() bool { return r.Status == VerifyOK }

// VerifyCredential authenticates a stored credential against a repository and
// records the outcome.
//
// repository may be empty, in which case the credential's own remembered probe
// repository is used. With neither, there is nothing meaningful to test — a
// token is scoped per repository, so "can reach github.com" says nothing about
// whether it can read the repo it is attached to — and the result is recorded
// as skipped rather than as a pass the operator would wrongly trust.
func (i *Importer) VerifyCredential(ctx context.Context, credentialID, repository string) (VerifyResult, error) {
	credential, err := i.Store.GetGitCredential(ctx, credentialID)
	if err != nil {
		return VerifyResult{}, errors.New("git credential not found")
	}
	repository = strings.TrimSpace(repository)
	if repository == "" {
		repository = credential.VerifyRepository
	}
	result := VerifyResult{CredentialID: credential.ID, Repository: repository, CheckedAt: time.Now().UTC()}
	if repository == "" {
		result.Status = VerifySkipped
		result.Error = "No repository to test against. Give this credential a repository URL so Asgard can prove it still works before a deployment needs it."
		return result, i.Store.RecordGitCredentialVerification(ctx, credential.ID, result.Status, result.Error, "")
	}
	source, err := composecfg.ValidateGitSource(repository, credential.Kind == store.GitCredentialSSH)
	if err != nil {
		result.Status = VerifyFailed
		result.Error = err.Error()
		return result, i.Store.RecordGitCredentialVerification(ctx, credential.ID, result.Status, result.Error, repository)
	}
	auth, err := i.resolveAuth(ctx, credential.ID)
	if err != nil {
		return result, err
	}
	refs, err := i.lsRemote(ctx, source.URL, auth)
	if err != nil {
		result.Status = VerifyFailed
		result.Error = err.Error()
	} else {
		result.Status, result.Refs = VerifyOK, refs
	}
	return result, i.Store.RecordGitCredentialVerification(ctx, credential.ID, result.Status, result.Error, repository)
}

// lsRemote asks the remote for its refs using the credential's environment. It
// transfers no objects, so it is cheap enough to run on a schedule against
// every stored credential.
func (i *Importer) lsRemote(ctx context.Context, url string, auth *gitAuth) (int, error) {
	environment, cleanup, err := i.gitEnvironment(auth)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--", url)
	cmd.Env = append(os.Environ(), environment...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := sanitizeOutput(output)
		if message == "" {
			message = err.Error()
		}
		if ctx.Err() != nil {
			message = "the remote did not answer within " + verifyTimeout.String()
		}
		if len(message) > maxVerifyError {
			message = message[:maxVerifyError]
		}
		return 0, fmt.Errorf("%s", message)
	}
	refs := 0
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) != "" {
			refs++
		}
	}
	return refs, nil
}

// VerifyAllCredentials re-checks every stored credential that has a repository
// to test against, and returns the results. Credentials with nothing to test
// are recorded as skipped so the gap itself stays visible.
func (i *Importer) VerifyAllCredentials(ctx context.Context) ([]VerifyResult, error) {
	items, err := i.Store.ListGitCredentials(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]VerifyResult, 0, len(items))
	for _, item := range items {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		result, err := i.VerifyCredential(ctx, item.ID, "")
		if err != nil {
			result = VerifyResult{CredentialID: item.ID, Status: VerifyFailed, Error: err.Error(), CheckedAt: time.Now().UTC()}
		}
		results = append(results, result)
	}
	return results, nil
}

// DefaultVerifyRepository derives a probe repository from a project's source
// URL, so a credential attached during an import inherits something concrete to
// be tested against without the operator entering it twice.
func DefaultVerifyRepository(sourceType, sourceURL string) string {
	if !IsGitSource(sourceType) {
		return ""
	}
	return strings.TrimSpace(sourceURL)
}
