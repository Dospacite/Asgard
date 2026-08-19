package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// gitHostPattern matches a bare DNS hostname. It is defined here rather than
// reused from composecfg because composecfg already depends on this package.
var gitHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Credential kinds. A token authenticates HTTPS clones; an SSH key
// authenticates git@host clones.
const (
	GitCredentialToken = "token"
	GitCredentialSSH   = "ssh"
)

// GitCredential is the metadata half of a stored credential. The secret itself
// is never part of this struct so it cannot leak through an API response.
type GitCredential struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Username   string     `json:"username,omitempty"`
	Host       string     `json:"host,omitempty"`
	Hint       string     `json:"hint,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`

	// Verification state. LastUsedAt alone is actively misleading: a credential
	// that worked for one project shows a recent timestamp while being unable
	// to read the repository it is attached to somewhere else. These record
	// whether the credential has actually been proven against a repository, and
	// why it stopped working if it has.
	LastVerifiedAt   *time.Time `json:"lastVerifiedAt,omitempty"`
	LastVerifyStatus string     `json:"lastVerifyStatus,omitempty"`
	LastVerifyError  string     `json:"lastVerifyError,omitempty"`
	VerifyRepository string     `json:"verifyRepository,omitempty"`
}

// gitCredentialColumns is the projection every credential read shares.
const gitCredentialColumns = `id,name,kind,username,host,hint,created_at,updated_at,last_used_at,last_verified_at,last_verify_status,last_verify_error,verify_repository`

func (s *Store) CreateGitCredential(ctx context.Context, item GitCredential, ciphertext, nonce []byte) (GitCredential, error) {
	if item.ID == "" {
		item.ID = uuid.NewString()
	}
	now := Now()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO git_credentials(id,name,kind,username,host,hint,ciphertext,nonce,verify_repository,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.Name, item.Kind, item.Username, item.Host, item.Hint, ciphertext, nonce, item.VerifyRepository, now, now)
	if err != nil {
		return GitCredential{}, err
	}
	return s.GetGitCredential(ctx, item.ID)
}

func (s *Store) ListGitCredentials(ctx context.Context) ([]GitCredential, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+gitCredentialColumns+` FROM git_credentials ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []GitCredential{}
	for rows.Next() {
		item, err := scanGitCredential(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetGitCredential(ctx context.Context, idOrName string) (GitCredential, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+gitCredentialColumns+` FROM git_credentials WHERE id=? OR name=? COLLATE NOCASE`, idOrName, idOrName)
	return scanGitCredential(row)
}

// GitCredentialSecret returns the sealed secret for a credential so the caller
// can decrypt it immediately before use.
func (s *Store) GitCredentialSecret(ctx context.Context, id string) (ciphertext, nonce []byte, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM git_credentials WHERE id=?`, id).Scan(&ciphertext, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, errors.New("git credential not found")
	}
	return ciphertext, nonce, err
}

func (s *Store) TouchGitCredential(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE git_credentials SET last_used_at=? WHERE id=?`, Now(), id)
	return err
}

func (s *Store) DeleteGitCredential(ctx context.Context, id string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM git_credentials WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	// Projects keep their source URL and history; only the broken link is cleared.
	_, err = s.DB.ExecContext(ctx, `UPDATE projects SET source_credential_id='' WHERE source_credential_id=?`, id)
	return err
}

// ProjectsUsingGitCredential reports which projects were imported with a
// credential so a deletion preview can name them.
func (s *Store) ProjectsUsingGitCredential(ctx context.Context, id string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT slug FROM projects WHERE source_credential_id=? ORDER BY slug`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slugs := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGitCredential(row rowScanner) (GitCredential, error) {
	var item GitCredential
	var created, updated string
	var lastUsed, lastVerified sql.NullString
	if err := row.Scan(&item.ID, &item.Name, &item.Kind, &item.Username, &item.Host, &item.Hint, &created, &updated, &lastUsed, &lastVerified, &item.LastVerifyStatus, &item.LastVerifyError, &item.VerifyRepository); err != nil {
		return GitCredential{}, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	item.LastUsedAt = parseTimePtr(lastUsed)
	item.LastVerifiedAt = parseTimePtr(lastVerified)
	return item, nil
}

// UpdateGitCredential rotates a credential in place, keeping its id.
//
// Keeping the id is the whole point. Without it, replacing a leaked or expired
// token means minting a new credential and then re-pointing every project at
// it — and until this existed there was no way to re-point a project at all, so
// the correct response to a compromised secret was not expressible through the
// product. Passing a nil ciphertext keeps the stored secret and updates only
// the metadata.
//
// Any rotation clears the previous verification result: the old outcome
// describes a secret that is no longer there, and reporting it against the new
// one is exactly the lie this whole change exists to remove.
func (s *Store) UpdateGitCredential(ctx context.Context, item GitCredential, ciphertext, nonce []byte) (GitCredential, error) {
	now := Now()
	var result sql.Result
	var err error
	if ciphertext == nil {
		result, err = s.DB.ExecContext(ctx, `UPDATE git_credentials SET name=?,username=?,host=?,verify_repository=?,updated_at=? WHERE id=?`,
			item.Name, item.Username, item.Host, item.VerifyRepository, now, item.ID)
	} else {
		result, err = s.DB.ExecContext(ctx, `UPDATE git_credentials SET name=?,username=?,host=?,hint=?,verify_repository=?,ciphertext=?,nonce=?,updated_at=?,last_verified_at=NULL,last_verify_status='',last_verify_error='' WHERE id=?`,
			item.Name, item.Username, item.Host, item.Hint, item.VerifyRepository, ciphertext, nonce, now, item.ID)
	}
	if err != nil {
		return GitCredential{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return GitCredential{}, sql.ErrNoRows
	}
	return s.GetGitCredential(ctx, item.ID)
}

// RecordGitCredentialVerification stores the outcome of a verification attempt.
// A skipped check does not count as a verification, so it leaves the timestamp
// alone rather than making an untested credential look freshly proven.
func (s *Store) RecordGitCredentialVerification(ctx context.Context, id, status, message, repository string) error {
	if status == "skipped" {
		_, err := s.DB.ExecContext(ctx, `UPDATE git_credentials SET last_verify_status=?,last_verify_error=? WHERE id=?`, status, message, id)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE git_credentials SET last_verified_at=?,last_verify_status=?,last_verify_error=?,verify_repository=CASE WHEN ?='' THEN verify_repository ELSE ? END WHERE id=?`,
		Now(), status, message, repository, repository, id)
	return err
}

// SetProjectSourceCredential re-points a project at a different stored
// credential, or at none. It is how a rotated secret reaches the projects that
// depend on it without hand-editing the database.
func (s *Store) SetProjectSourceCredential(ctx context.Context, projectID, credentialID string) error {
	if credentialID != "" {
		if _, err := s.GetGitCredential(ctx, credentialID); err != nil {
			return errors.New("git credential not found")
		}
	}
	sourceType := "git"
	if credentialID != "" {
		sourceType = "git-private"
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE projects SET source_credential_id=?,source_type=?,updated_at=? WHERE id=? AND source_type IN ('git','git-private')`, credentialID, sourceType, Now(), projectID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errors.New("only a project imported from Git has a source credential")
	}
	return nil
}

const maxGitSecretBytes = 64 << 10

// NormalizeGitCredential validates operator input and derives the non-secret
// hint shown in the UI. The hint is a truncated digest for tokens and the key
// comment for SSH keys, so a credential stays recognizable without exposing it.
func NormalizeGitCredential(name, kind, username, host, secret, verifyRepository string) (GitCredential, []byte, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return GitCredential{}, nil, errors.New("name is required and limited to 100 characters")
	}
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind != GitCredentialToken && kind != GitCredentialSSH {
		return GitCredential{}, nil, errors.New("kind must be token or ssh")
	}
	if len(secret) == 0 || len(secret) > maxGitSecretBytes {
		return GitCredential{}, nil, errors.New("secret is required and limited to 64 KiB")
	}
	host = strings.TrimSpace(strings.ToLower(host))
	if host != "" && (len(host) > 253 || !gitHostPattern.MatchString(host)) {
		return GitCredential{}, nil, errors.New("host must be a bare hostname such as github.com")
	}
	item := GitCredential{Name: name, Kind: kind, Host: host, VerifyRepository: strings.TrimSpace(verifyRepository)}
	if kind == GitCredentialToken {
		secret = strings.TrimSpace(secret)
		if strings.ContainsAny(secret, "\r\n") {
			return GitCredential{}, nil, errors.New("token must be a single line")
		}
		item.Username = strings.TrimSpace(username)
		if item.Username == "" {
			item.Username = "x-access-token"
		}
		digest := sha256.Sum256([]byte(secret))
		item.Hint = "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])[:12]
		return item, []byte(secret), nil
	}
	if !strings.Contains(secret, "PRIVATE KEY") {
		return GitCredential{}, nil, errors.New("paste a PEM-encoded private key, including its BEGIN and END lines")
	}
	item.Hint = "private key"
	return item, []byte(secret), nil
}

// NormalizeGitCredentialUpdate validates a rotation. The kind is fixed at
// creation — a token and an SSH key are authenticated in completely different
// ways, and silently switching one for the other under a name projects already
// reference would change what a clone does without anyone asking for it. An
// empty secret means metadata-only, and returns a nil secret.
func NormalizeGitCredentialUpdate(current GitCredential, name, username, host, secret, verifyRepository string) (GitCredential, []byte, error) {
	item, plaintext, err := NormalizeGitCredential(name, current.Kind, username, host, orPlaceholder(secret, current.Kind), verifyRepository)
	if err != nil {
		return GitCredential{}, nil, err
	}
	item.ID = current.ID
	if strings.TrimSpace(secret) == "" {
		// Metadata-only: keep the stored secret and its hint untouched.
		item.Hint = current.Hint
		return item, nil, nil
	}
	return item, plaintext, nil
}

// orPlaceholder lets a metadata-only update reuse the create-time validation
// without a real secret. The placeholder is never stored: the caller discards
// the returned plaintext whenever secret is blank.
func orPlaceholder(secret, kind string) string {
	if strings.TrimSpace(secret) != "" {
		return secret
	}
	if kind == GitCredentialSSH {
		return "-----BEGIN PRIVATE KEY-----\nplaceholder\n-----END PRIVATE KEY-----"
	}
	return "placeholder"
}
