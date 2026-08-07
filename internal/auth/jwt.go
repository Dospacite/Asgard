package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Claims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	JWTID    string `json:"jti"`
	Username string `json:"username,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Type     string `json:"typ"`
}

type Signer struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
	keyID   string
	issuer  string
}

func LoadOrCreateSigner(path, issuer string) (*Signer, error) {
	bytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			return nil, err
		}
		bytes = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(path, bytes, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(bytes)
	if block == nil {
		return nil, errors.New("invalid JWT private key PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	private, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("JWT key is not Ed25519")
	}
	public := private.Public().(ed25519.PublicKey)
	kidBytes := public
	if len(kidBytes) > 9 {
		kidBytes = kidBytes[:9]
	}
	return &Signer{private: private, public: public, keyID: base64.RawURLEncoding.EncodeToString(kidBytes), issuer: issuer}, nil
}

func (s *Signer) Sign(subject, audience, username, scope, tokenType string, ttl time.Duration) (string, Claims, error) {
	now := time.Now().UTC()
	claims := Claims{Issuer: s.issuer, Subject: subject, Audience: audience, Expires: now.Add(ttl).Unix(), IssuedAt: now.Unix(), JWTID: uuid.NewString(), Username: username, Scope: scope, Type: tokenType}
	header := map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": s.keyID}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sig := ed25519.Sign(s.private, []byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), claims, nil
}

func (s *Signer) Verify(token, audience, tokenType string) (Claims, error) {
	var claims Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, errors.New("invalid signature encoding")
	}
	if !ed25519.Verify(s.public, []byte(parts[0]+"."+parts[1]), sig) {
		return claims, errors.New("invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, err
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, err
	}
	now := time.Now().Unix()
	if claims.Issuer != s.issuer || claims.Audience != audience || claims.Type != tokenType {
		return claims, errors.New("token claims do not match")
	}
	if claims.Expires <= now || claims.IssuedAt > now+60 {
		return claims, errors.New("token expired or not yet valid")
	}
	return claims, nil
}

func (s *Signer) JWKS() map[string]any {
	return map[string]any{"keys": []map[string]any{{"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": s.keyID, "x": base64.RawURLEncoding.EncodeToString(s.public)}}}
}

func (s *Signer) KeyID() string  { return s.keyID }
func (s *Signer) Issuer() string { return s.issuer }

func randomToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func HashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (c Claims) ValidateScope(required string) error {
	for _, scope := range strings.Fields(c.Scope) {
		if scope == required {
			return nil
		}
	}
	return fmt.Errorf("scope %q is required", required)
}
