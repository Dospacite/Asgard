package operations

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/store"
)

func TestPanickingHandlerFailsItsOperation(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := New(database, 1)
	manager.Register("explode", func(context.Context, store.Operation) error { panic("boom") })
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	op, err := database.CreateOperation(ctx, store.Operation{ID: uuid.NewString(), Kind: "explode", TargetType: "system", TargetID: "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	manager.Enqueue(op.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := database.GetOperation(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "failed" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation never failed")
}
