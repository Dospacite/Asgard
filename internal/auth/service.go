package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/store"
)

const (
	AccessCookie  = "asgard_access"
	RefreshCookie = "asgard_refresh"
	CSRFCookie    = "asgard_csrf"
)

type Service struct {
	store      *store.Store
	signer     *Signer
	publicURL  string
	secure     bool
	accessTTL  time.Duration
	refreshTTL time.Duration
}

type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

type Identity struct {
	UserID    string
	Username  string
	ActorType string
	Scope     string
	Claims    Claims
}
type contextKey int

const identityKey contextKey = 1

func New(store *store.Store, signer *Signer, publicURL string, secure bool, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{store: store, signer: signer, publicURL: publicURL, secure: secure, accessTTL: accessTTL, refreshTTL: refreshTTL}
}

func (s *Service) Signer() *Signer { return s.signer }

func (s *Service) CreateUser(ctx context.Context, username, password string) (User, error) {
	username = strings.TrimSpace(username)
	if len(username) < 2 || len(username) > 64 {
		return User{}, errors.New("username must be 2 to 64 characters")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return User{}, errors.New("username may only contain letters, numbers, dot, dash, and underscore")
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	id := uuid.NewString()
	now := store.Now()
	_, err = s.store.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,?,?)`, id, username, hash, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, fmt.Errorf("user %q already exists", username)
		}
		return User{}, err
	}
	return User{ID: id, Username: username, CreatedAt: time.Now().UTC()}, nil
}

func (s *Service) ResetPassword(ctx context.Context, username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	result, err := s.store.DB.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE username=? COLLATE NOCASE`, hash, store.Now(), username)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	_, _ = s.store.DB.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=(SELECT id FROM users WHERE username=? COLLATE NOCASE) AND revoked_at IS NULL`, store.Now(), username)
	return nil
}

func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := s.store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

func (s *Service) Authenticate(ctx context.Context, username, password string) (User, error) {
	var user User
	var hash, created string
	var last sql.NullString
	err := s.store.DB.QueryRowContext(ctx, `SELECT id,username,password_hash,created_at,last_login_at FROM users WHERE username=? COLLATE NOCASE`, strings.TrimSpace(username)).Scan(&user.ID, &user.Username, &hash, &created, &last)
	if err != nil || !VerifyPassword(hash, password) {
		return User{}, errors.New("invalid username or password")
	}
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if last.Valid {
		v, _ := time.Parse(time.RFC3339Nano, last.String)
		user.LastLoginAt = &v
	}
	_, _ = s.store.DB.ExecContext(ctx, `UPDATE users SET last_login_at=?,updated_at=? WHERE id=?`, store.Now(), store.Now(), user.ID)
	return user, nil
}

func (s *Service) IssueSession(ctx context.Context, user User) (access, refresh, csrf string, err error) {
	access, _, err = s.signer.Sign(user.ID, s.publicURL, user.Username, "", "session", s.accessTTL)
	if err != nil {
		return
	}
	refresh, err = randomToken(48)
	if err != nil {
		return
	}
	csrf, err = randomToken(32)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	_, err = s.store.DB.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,family_id,expires_at,created_at) VALUES(?,?,?,?,?,?)`, uuid.NewString(), user.ID, HashToken(refresh), uuid.NewString(), now.Add(s.refreshTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return
}

func (s *Service) Refresh(ctx context.Context, oldToken string) (access, newRefresh, csrf string, user User, err error) {
	var sessionID, userID, familyID, expiry string
	var used, revoked sql.NullString
	err = s.store.DB.QueryRowContext(ctx, `SELECT id,user_id,family_id,expires_at,used_at,revoked_at FROM sessions WHERE token_hash=?`, HashToken(oldToken)).Scan(&sessionID, &userID, &familyID, &expiry, &used, &revoked)
	if err != nil {
		return "", "", "", user, errors.New("invalid refresh token")
	}
	expires, _ := time.Parse(time.RFC3339Nano, expiry)
	if revoked.Valid || used.Valid || time.Now().After(expires) {
		_, _ = s.store.DB.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE family_id=? AND revoked_at IS NULL`, store.Now(), familyID)
		return "", "", "", user, errors.New("refresh token has expired or was already used")
	}
	var created string
	var last sql.NullString
	err = s.store.DB.QueryRowContext(ctx, `SELECT id,username,created_at,last_login_at FROM users WHERE id=?`, userID).Scan(&user.ID, &user.Username, &created, &last)
	if err != nil {
		return
	}
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	newRefresh, err = randomToken(48)
	if err != nil {
		return
	}
	csrf, err = randomToken(32)
	if err != nil {
		return
	}
	newID := uuid.NewString()
	now := time.Now().UTC()
	tx, txErr := s.store.DB.BeginTx(ctx, nil)
	if txErr != nil {
		err = txErr
		return
	}
	defer tx.Rollback()
	result, txErr := tx.ExecContext(ctx, `UPDATE sessions SET used_at=?,replaced_by=? WHERE id=? AND used_at IS NULL AND revoked_at IS NULL`, now.Format(time.RFC3339Nano), newID, sessionID)
	if txErr != nil {
		err = txErr
		return
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		err = errors.New("refresh token reuse detected")
		return
	}
	_, txErr = tx.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,family_id,expires_at,created_at) VALUES(?,?,?,?,?,?)`, newID, user.ID, HashToken(newRefresh), familyID, now.Add(s.refreshTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if txErr != nil {
		err = txErr
		return
	}
	if txErr = tx.Commit(); txErr != nil {
		err = txErr
		return
	}
	access, _, err = s.signer.Sign(user.ID, s.publicURL, user.Username, "", "session", s.accessTTL)
	return
}

func (s *Service) RevokeSession(ctx context.Context, refresh string) {
	if refresh != "" {
		_, _ = s.store.DB.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`, store.Now(), HashToken(refresh))
	}
}

func (s *Service) SetCookies(w http.ResponseWriter, access, refresh, csrf string) {
	http.SetCookie(w, &http.Cookie{Name: AccessCookie, Value: access, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(s.accessTTL.Seconds())})
	http.SetCookie(w, &http.Cookie{Name: RefreshCookie, Value: refresh, Path: "/api/v1/auth", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(s.refreshTTL.Seconds())})
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Value: csrf, Path: "/", HttpOnly: false, Secure: s.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(s.refreshTTL.Seconds())})
}

func (s *Service) ClearCookies(w http.ResponseWriter) {
	for _, cookie := range []*http.Cookie{{Name: AccessCookie, Path: "/", HttpOnly: true}, {Name: RefreshCookie, Path: "/api/v1/auth", HttpOnly: true}, {Name: CSRFCookie, Path: "/"}} {
		cookie.Value = ""
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0)
		cookie.Secure = s.secure
		cookie.SameSite = http.SameSiteStrictMode
		http.SetCookie(w, cookie)
	}
}

func IdentityFrom(ctx context.Context) (Identity, bool) {
	v, ok := ctx.Value(identityKey).(Identity)
	return v, ok
}

func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey, identity)
}

func (s *Service) IdentityRequest(r *http.Request) (Identity, error) {
	var token string
	if header := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(header), "bearer ") {
		token = strings.TrimSpace(header[7:])
	} else if cookie, err := r.Cookie(AccessCookie); err == nil {
		token = cookie.Value
	}
	if token == "" {
		return Identity{}, errors.New("authentication required")
	}
	claims, err := s.signer.Verify(token, s.publicURL, "session")
	if err != nil {
		return Identity{}, err
	}
	return Identity{UserID: claims.Subject, Username: claims.Username, ActorType: "user", Claims: claims}, nil
}

func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := s.IdentityRequest(r)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

func (s *Service) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(CSRFCookie)
		if err != nil || cookie.Value == "" || subtleCompare(cookie.Value, r.Header.Get("X-CSRF-Token")) == false {
			http.Error(w, "CSRF token missing or invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	var result byte
	for i := range a {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

func NewCSRFToken() string {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}
