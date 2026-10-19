package dockerx

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func contextEntries(t *testing.T, root, dockerfile string) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeBuildContext(&buf, root, dockerfile); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&buf)
	names := []string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			names = append(names, header.Name)
		}
	}
	sort.Strings(names)
	return names
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildContextHonorsDockerignore(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".dockerignore":      "*.md\nDockerfile\n.dockerignore\ndist\ndocs/**\n!docs/keep.txt\n",
		"Dockerfile":         "FROM scratch\n",
		"README.md":          "readme",
		"main.go":            "package main",
		"dist/bundle.js":     "built",
		"docs/guide.txt":     "guide",
		"docs/keep.txt":      "keep",
		"src/notes.md":       "notes",
		"src/app/handler.go": "package app",
	})
	got := contextEntries(t, root, "Dockerfile")
	// `*.md` matches only at the root, as in Docker.
	want := []string{".dockerignore", "Dockerfile", "docs/keep.txt", "main.go", "src/app/handler.go", "src/notes.md"}
	if !equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}

func TestBuildContextPrefersDockerfileSpecificIgnore(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".dockerignore":                        "web\n",
		"services/api/Dockerfile":              "FROM scratch\n",
		"services/api/Dockerfile.dockerignore": "web\nservices/api/tests\n",
		"services/api/main.py":                 "app",
		"services/api/tests/test_main.py":      "test",
		"web/index.html":                       "page",
	})
	got := contextEntries(t, root, "services/api/Dockerfile")
	// The root .dockerignore is not consulted, so it is an ordinary file here.
	want := []string{".dockerignore", "services/api/Dockerfile", "services/api/Dockerfile.dockerignore", "services/api/main.py"}
	if !equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}

func TestBuildContextKeepsDockerfileInsideIgnoredDirectory(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".dockerignore":    "build\n",
		"build/Dockerfile": "FROM scratch\n",
		"build/cache.bin":  "junk",
		"app.txt":          "app",
	})
	got := contextEntries(t, root, "build/Dockerfile")
	want := []string{".dockerignore", "app.txt", "build/Dockerfile"}
	if !equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}

func TestBuildContextWithoutDockerignoreSendsEverything(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"Dockerfile": "FROM scratch\n", "README.md": "readme", "node_modules/x/index.js": "dep"})
	got := contextEntries(t, root, "Dockerfile")
	want := []string{"Dockerfile", "README.md"}
	if !equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
