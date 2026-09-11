package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// LogCursor is how far the log collector has read a container's output.
//
// It carries the last line as well as its timestamp because Docker's since
// filter is inclusive: the next read starts at the record already shipped, and
// without the line to compare against, a repeated record is indistinguishable
// from a genuine second line written in the same nanosecond.
type LogCursor struct {
	ContainerID string
	Time        time.Time
	Stream      string
	Line        string
}

// LogCursor returns the checkpoint for a container, if one has been stored.
func (s *Store) LogCursor(ctx context.Context, containerID string) (LogCursor, bool, error) {
	var lastTS, stream, line string
	err := s.DB.QueryRowContext(ctx, `SELECT last_ts,last_stream,last_line FROM log_cursors WHERE container_id=?`, containerID).Scan(&lastTS, &stream, &line)
	if errors.Is(err, sql.ErrNoRows) {
		return LogCursor{}, false, nil
	}
	if err != nil {
		return LogCursor{}, false, err
	}
	at, err := time.Parse(time.RFC3339Nano, lastTS)
	if err != nil {
		// An unreadable checkpoint is worth no more than a missing one: the
		// collector starts from its lookback window and re-reads.
		return LogCursor{}, false, nil
	}
	return LogCursor{ContainerID: containerID, Time: at, Stream: stream, Line: line}, true, nil
}

// SetLogCursor records that everything up to and including this frame has been
// handed to the sink.
func (s *Store) SetLogCursor(ctx context.Context, cursor LogCursor) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO log_cursors(container_id,last_ts,last_stream,last_line,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(container_id) DO UPDATE SET last_ts=excluded.last_ts,last_stream=excluded.last_stream,last_line=excluded.last_line,updated_at=excluded.updated_at`,
		cursor.ContainerID, cursor.Time.UTC().Format(time.RFC3339Nano), cursor.Stream, cursor.Line, Now())
	return err
}

// PruneLogCursors forgets containers that no longer exist. Containers are
// recreated on every release, so without this the table grows by one row per
// service per deployment forever.
func (s *Store) PruneLogCursors(ctx context.Context, keep []string) (int64, error) {
	if len(keep) == 0 {
		result, err := s.DB.ExecContext(ctx, `DELETE FROM log_cursors`)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, 0, len(keep))
	for _, id := range keep {
		args = append(args, id)
	}
	result, err := s.DB.ExecContext(ctx, `DELETE FROM log_cursors WHERE container_id NOT IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
