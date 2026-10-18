package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

// A validation failure has to name the path it rejected and why, because that
// text is what an operator — or an importing agent — acts on. The reasons live
// in the contract rather than in the transport, so this checks that both the
// path and the refusal survive into the error.
func TestFinishReportsComposeValidationDetails(t *testing.T) {
	root := t.TempDir()
	compose := `services:
  web:
    image: nginx:1.27
    privileged: true
    imagee: nginx:1.27
`
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}

	project := store.Project{ID: "project-id", Slug: "demo"}
	result, err := (&Importer{}).finish(context.Background(), &project, root, "compose.yaml")
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if result.Valid {
		t.Fatal("invalid Compose file reported as valid")
	}
	for _, want := range []string{
		"services.web.privileged:",
		"services.web.imagee:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error %q carries no reason", err)
	}
}
