package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBoxRoundTripsAcrossReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "secrets.key")
	box, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := box.Seal([]byte("ghp_example_token"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	reopened, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := reopened.Open(ciphertext, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "ghp_example_token" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

func TestBoxRejectsTamperedCiphertext(t *testing.T) {
	box, err := LoadOrCreate(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := box.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[0] ^= 0xff
	if _, err := box.Open(ciphertext, nonce); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestBoxRejectsForeignKey(t *testing.T) {
	first, err := LoadOrCreate(filepath.Join(t.TempDir(), "a.key"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(filepath.Join(t.TempDir(), "b.key"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := first.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Open(ciphertext, nonce); err == nil {
		t.Fatal("ciphertext decrypted under a different key")
	}
}
