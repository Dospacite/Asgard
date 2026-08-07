package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

func TestFinishReportsComposeValidationDetails(t *testing.T) {
	root := t.TempDir()
	compose := `x-flags:
  FEATURE: enabled
services:
  web:
    image: nginx:1.27
    networks: [default]
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
		"compose.x-flags: This field is not supported by Asgard's safe Compose contract.",
		"services.web.networks: This field is not supported by Asgard's safe Compose contract.",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}
