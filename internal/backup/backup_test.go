package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyChecksumRejectsChangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume.tar.gz")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("snapshot"))
	if err := verifyChecksum(path, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("snapsho"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(path, hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("truncated backup accepted")
	}
	if err := verifyChecksum(path, ""); err != nil {
		t.Fatalf("backup without a recorded digest refused: %v", err)
	}
}
