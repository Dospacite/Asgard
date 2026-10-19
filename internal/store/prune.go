package store

import (
	"context"
	"time"
)

// Retention for rows nothing reads once they are spent. Without it a control
// plane kept every expired session, every rotated OAuth refresh token, and
// every line of every operation log since it was installed.
const (
	// Spent credentials are kept a day past expiry or revocation, so reuse of
	// a rotated token is still detected and its whole family revoked.
	spentCredentialGrace = 24 * time.Hour
	// Operation logs explain recent deployments; older ones only take space.
	// The operations themselves, and the release history, are kept.
	OperationLogRetention = 30 * 24 * time.Hour
)

// PruneResult counts the rows one pass removed.
type PruneResult struct {
	Sessions      int64
	OAuthCodes    int64
	OAuthTokens   int64
	OperationLogs int64
}

// Prune deletes expired or spent credentials and old operation logs.
func (s *Store) Prune(ctx context.Context, now time.Time) (PruneResult, error) {
	var result PruneResult
	cutoff := now.UTC().Add(-spentCredentialGrace).Format(time.RFC3339Nano)
	logCutoff := now.UTC().Add(-OperationLogRetention).Format(time.RFC3339Nano)
	steps := []struct {
		count *int64
		query string
		args  []any
	}{
		// A rotated refresh token stays until it expires, which is as long as
		// presenting it again could still mean something: reuse detection.
		{&result.Sessions, `DELETE FROM sessions WHERE expires_at < ? OR revoked_at < ?`, []any{cutoff, cutoff}},
		{&result.OAuthCodes, `DELETE FROM oauth_codes WHERE expires_at < ?`, []any{cutoff}},
		{&result.OAuthTokens, `DELETE FROM oauth_tokens WHERE expires_at < ? OR revoked_at < ?`, []any{cutoff, cutoff}},
		{&result.OperationLogs, `DELETE FROM operation_logs WHERE operation_id IN (SELECT id FROM operations WHERE completed_at IS NOT NULL AND completed_at < ?)`, []any{logCutoff}},
	}
	for _, step := range steps {
		res, err := s.DB.ExecContext(ctx, step.query, step.args...)
		if err != nil {
			return result, err
		}
		*step.count, _ = res.RowsAffected()
	}
	return result, nil
}
