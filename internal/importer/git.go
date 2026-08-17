package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

// gitAuth carries everything a single clone needs to authenticate. Secrets are
// written to a private temporary directory and passed to git through helper
// scripts, never through argv or the remote URL, so they cannot be observed in
// the process table or persisted into the cloned repository's config.
type gitAuth struct {
	credential store.GitCredential
	secret     []byte
}

// resolveAuth loads and decrypts the requested credential.
func (i *Importer) resolveAuth(ctx context.Context, credentialID string) (*gitAuth, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return nil, nil
	}
	if i.Store == nil || i.Secrets == nil {
		return nil, errors.New("credential storage is unavailable")
	}
	credential, err := i.Store.GetGitCredential(ctx, credentialID)
	if err != nil {
		return nil, errors.New("git credential not found")
	}
	ciphertext, nonce, err := i.Store.GitCredentialSecret(ctx, credential.ID)
	if err != nil {
		return nil, err
	}
	secret, err := i.Secrets.Open(ciphertext, nonce)
	if err != nil {
		return nil, err
	}
	return &gitAuth{credential: credential, secret: secret}, nil
}

// gitEnvironment materializes the credential on disk and returns the extra
// environment for the clone plus a cleanup function.
func (i *Importer) gitEnvironment(auth *gitAuth) ([]string, func(), error) {
	base := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_LFS_SKIP_SMUDGE=1"}
	if auth == nil {
		return base, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "asgard-git-")
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	switch auth.credential.Kind {
	case store.GitCredentialSSH:
		keyPath := filepath.Join(dir, "id")
		key := auth.secret
		if len(key) > 0 && key[len(key)-1] != '\n' {
			key = append(append([]byte{}, key...), '\n')
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
		knownHosts := i.knownHostsPath()
		command := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s", shellQuote(keyPath), shellQuote(knownHosts))
		return append(base, "GIT_SSH_COMMAND="+command), cleanup, nil
	case store.GitCredentialToken:
		username := auth.credential.Username
		if username == "" {
			username = "x-access-token"
		}
		if err := os.WriteFile(filepath.Join(dir, "username"), []byte(username), 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "password"), auth.secret, 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
		askpass := filepath.Join(dir, "askpass.sh")
		script := "#!/bin/sh\ncase \"$1\" in\n  *[Uu]sername*) cat " + shellQuote(filepath.Join(dir, "username")) + " ;;\n  *) cat " + shellQuote(filepath.Join(dir, "password")) + " ;;\nesac\n"
		if err := os.WriteFile(askpass, []byte(script), 0o700); err != nil {
			cleanup()
			return nil, nil, err
		}
		return append(base, "GIT_ASKPASS="+askpass, "SSH_ASKPASS="+askpass), cleanup, nil
	}
	cleanup()
	return nil, nil, fmt.Errorf("unsupported credential kind %q", auth.credential.Kind)
}

func (i *Importer) knownHostsPath() string {
	dir := i.DataDir
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, "keys", "git_known_hosts")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = os.WriteFile(path, nil, 0o600)
	}
	return path
}

// clone runs the shallow clone that backs every Git import.
func (i *Importer) clone(ctx context.Context, source composecfg.GitSource, ref, root string, auth *gitAuth) error {
	environment, cleanup, err := i.gitEnvironment(auth)
	if err != nil {
		return err
	}
	defer cleanup()
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", source.URL, root)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), environment...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("clone failed: %s", sanitizeOutput(output))
	}
	return nil
}

// headCommit reads the commit a clone landed on. It runs before the clone's
// .git directory is discarded, and is the only record of which revision the
// working tree that deployments build actually holds.
func (i *Importer) headCommit(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read cloned revision: %s", sanitizeOutput(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
