package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeTarGz(t *testing.T, entries []tar.Header, bodies map[string]string) string {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gz)
	for _, header := range entries {
		body := bodies[header.Name]
		header.Size = int64(len(body))
		if err := archive.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "project.tar.gz")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractArchiveTarGz(t *testing.T) {
	path := writeTarGz(t,
		[]tar.Header{
			{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "app/compose.yaml", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "app/src/index.js", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		map[string]string{"app/compose.yaml": "services: {}\n", "app/src/index.js": "console.log(1)\n"},
	)
	target := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ExtractArchive(path, target, "project.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "app", "compose.yaml")); err != nil || string(data) != "services: {}\n" {
		t.Fatalf("compose not extracted: %v %q", err, data)
	}
	if err := flattenSingleRoot(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "compose.yaml")); err != nil {
		t.Fatalf("single wrapper directory was not flattened: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "src", "index.js")); err != nil {
		t.Fatalf("nested file lost during flattening: %v", err)
	}
	if found, err := FindCompose(target); err != nil || found != "compose.yaml" {
		t.Fatalf("FindCompose = %q, %v", found, err)
	}
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	path := writeTarGz(t,
		[]tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644}},
		map[string]string{"../escape": "bad"},
	)
	if err := ExtractArchive(path, t.TempDir(), "project.tar.gz"); err == nil {
		t.Fatal("tar traversal accepted")
	}
}

func TestExtractArchiveRejectsSymlink(t *testing.T) {
	path := writeTarGz(t,
		[]tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}},
		map[string]string{},
	)
	if err := ExtractArchive(path, t.TempDir(), "project.tar.gz"); err == nil {
		t.Fatal("tar symlink accepted")
	}
}

func TestDetectFormatPrefersContentOverName(t *testing.T) {
	path := writeTarGz(t, []tar.Header{{Name: "compose.yaml", Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{"compose.yaml": "services: {}\n"})
	format, err := DetectFormat(path, "mislabeled.zip")
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatTarGz {
		t.Fatalf("format = %q, want %q", format, FormatTarGz)
	}
}

func TestHasArchiveExtension(t *testing.T) {
	for _, name := range []string{"a.zip", "a.tar", "a.TAR.GZ", "a.tgz", "a.tar.bz2", "a.tar.xz", "a.tar.zst", "a.tzst"} {
		if !HasArchiveExtension(name) {
			t.Fatalf("%q rejected", name)
		}
	}
	for _, name := range []string{"a.rar", "a.7z", "a.gz", "compose.yaml"} {
		if HasArchiveExtension(name) {
			t.Fatalf("%q accepted", name)
		}
	}
}
