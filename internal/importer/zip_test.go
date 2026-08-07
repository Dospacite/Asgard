package importer

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractZIPRejectsTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entry, err := archive.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("bad"))
	_ = archive.Close()
	_ = file.Close()
	if err := ExtractZIP(path, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("zip traversal accepted")
	}
}
