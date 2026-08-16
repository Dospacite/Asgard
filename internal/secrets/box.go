// Package secrets provides authenticated encryption for values Asgard must be
// able to read back, such as Git credentials. It is deliberately separate from
// password hashing, which is one-way by design.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const keyBytes = 32

// Box seals and opens values with AES-256-GCM under a host-local key file.
type Box struct{ aead cipher.AEAD }

// LoadOrCreate reads the key at path, generating a fresh one on first use. The
// key file is written before any secret depends on it so an interrupted first
// start cannot leave undecryptable ciphertext behind.
func LoadOrCreate(path string) (*Box, error) {
	key, err := readKey(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, keyBytes)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err = os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write secret key: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func readKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("secret key at %s is not hex-encoded: %w", path, err)
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("secret key at %s must be %d bytes", path, keyBytes)
	}
	return key, nil
}

// Seal encrypts plaintext and returns the ciphertext with its fresh nonce.
func (b *Box) Seal(plaintext []byte) ([]byte, []byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return b.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// Open authenticates and decrypts a previously sealed value.
func (b *Box) Open(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != b.aead.NonceSize() {
		return nil, errors.New("stored secret has an invalid nonce")
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("stored secret could not be decrypted with the current key")
	}
	return plaintext, nil
}
