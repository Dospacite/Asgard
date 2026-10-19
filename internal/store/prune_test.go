package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneRemovesSpentRowsOnly(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
	future := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES('u','admin','x',?,?)`, old, old)
	mustExec(`INSERT INTO sessions(id,user_id,token_hash,family_id,expires_at,created_at) VALUES('expired','u','a','f',?,?)`, old, old)
	mustExec(`INSERT INTO sessions(id,user_id,token_hash,family_id,expires_at,created_at) VALUES('live','u','b','g',?,?)`, future, old)
	mustExec(`INSERT INTO operations(id,kind,target_type,target_id,status,created_at,completed_at) VALUES('old','deploy','project','p','succeeded',?,?)`, old, old)
	mustExec(`INSERT INTO operations(id,kind,target_type,target_id,status,created_at) VALUES('running','deploy','project','p','running',?)`, old)
	mustExec(`INSERT INTO operation_logs(operation_id,level,message,created_at) VALUES('old','info','gone',?)`, old)
	mustExec(`INSERT INTO operation_logs(operation_id,level,message,created_at) VALUES('running','info','kept',?)`, old)

	result, err := database.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 1 || result.OperationLogs != 1 {
		t.Fatalf("result = %+v", result)
	}
	var sessions, logs, operations int
	_ = database.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	_ = database.DB.QueryRow(`SELECT COUNT(*) FROM operation_logs`).Scan(&logs)
	_ = database.DB.QueryRow(`SELECT COUNT(*) FROM operations`).Scan(&operations)
	if sessions != 1 || logs != 1 || operations != 2 {
		t.Fatalf("sessions=%d logs=%d operations=%d", sessions, logs, operations)
	}
}
