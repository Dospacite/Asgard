package store

import (
	"context"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func storeCredential(t *testing.T, database *Store, name, secret string) GitCredential {
	t.Helper()
	item, plaintext, err := NormalizeGitCredential(name, GitCredentialToken, "", "github.com", secret, "https://github.com/owner/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	created, err := database.CreateGitCredential(context.Background(), item, plaintext, []byte("nonce"))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// Rotation has to keep the id. Minting a new credential instead leaves every
// project pointing at the old, dead secret — which is why replacing a leaked
// token used to require hand-editing the database.
func TestRotationKeepsTheIdAndReplacesTheSecret(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	created := storeCredential(t, database, "github-https", "old-token")

	item, secret, err := NormalizeGitCredentialUpdate(created, created.Name, "", "github.com", "new-token", "https://github.com/owner/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if secret == nil {
		t.Fatal("a rotation with a secret must return one to seal")
	}
	updated, err := database.UpdateGitCredential(ctx, item, secret, []byte("nonce2"))
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID {
		t.Fatalf("rotation changed the id: %s -> %s", created.ID, updated.ID)
	}
	if updated.Hint == created.Hint {
		t.Fatal("the hint should track the new secret")
	}
	ciphertext, _, err := database.GitCredentialSecret(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) != "new-token" {
		t.Fatalf("stored secret is %q, want the rotated one", ciphertext)
	}
}

// A rotated secret has never been proven, so carrying the old result forward
// would be exactly the lie this feature exists to remove.
func TestRotationClearsThePreviousVerification(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	created := storeCredential(t, database, "github-https", "old-token")
	if err := database.RecordGitCredentialVerification(ctx, created.ID, "ok", "", "https://github.com/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	verified, _ := database.GetGitCredential(ctx, created.ID)
	if verified.LastVerifiedAt == nil || verified.LastVerifyStatus != "ok" {
		t.Fatalf("setup failed: %#v", verified)
	}

	item, secret, err := NormalizeGitCredentialUpdate(verified, verified.Name, "", "github.com", "new-token", "")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := database.UpdateGitCredential(ctx, item, secret, []byte("nonce2"))
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastVerifiedAt != nil || updated.LastVerifyStatus != "" {
		t.Fatalf("a rotated credential must not inherit the old verification: %#v", updated)
	}
}

// Metadata-only updates keep the stored secret; nothing else can reach it.
func TestMetadataOnlyUpdateKeepsTheSecret(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	created := storeCredential(t, database, "github-https", "keep-me")

	item, secret, err := NormalizeGitCredentialUpdate(created, "renamed", "", "gitlab.com", "", "https://gitlab.com/owner/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if secret != nil {
		t.Fatal("a metadata-only update must not produce a secret to store")
	}
	updated, err := database.UpdateGitCredential(ctx, item, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" || updated.Host != "gitlab.com" {
		t.Fatalf("metadata was not applied: %#v", updated)
	}
	if updated.Hint != created.Hint {
		t.Fatalf("the hint must still describe the unchanged secret")
	}
	ciphertext, _, err := database.GitCredentialSecret(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) != "keep-me" {
		t.Fatalf("stored secret changed to %q", ciphertext)
	}
}

// A skipped check is not a verification. Stamping a timestamp for it would make
// an untested credential look freshly proven, which is the failure mode the
// whole feature is meant to remove.
func TestSkippedVerificationDoesNotStampATimestamp(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	created := storeCredential(t, database, "github-https", "token")
	if err := database.RecordGitCredentialVerification(ctx, created.ID, "skipped", "nothing to test against", ""); err != nil {
		t.Fatal(err)
	}
	item, _ := database.GetGitCredential(ctx, created.ID)
	if item.LastVerifiedAt != nil {
		t.Fatal("a skipped check must not count as a verification")
	}
	if item.LastVerifyStatus != "skipped" {
		t.Fatalf("status is %q", item.LastVerifyStatus)
	}
}

// Re-pointing a project at a different credential is the other half of a
// rotation; without it a replaced secret can never reach the projects using it.
func TestSetProjectSourceCredential(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	credential := storeCredential(t, database, "github-https", "token")
	project := Project{ID: "project-1", Slug: "example", Name: "Example", SourceType: "git", SourceURL: "https://github.com/owner/repo.git", SourcePath: "/tmp/example", ComposePath: "compose.yaml"}
	if err := database.CreateProject(ctx, project, nil); err != nil {
		t.Fatal(err)
	}

	if err := database.SetProjectSourceCredential(ctx, project.ID, credential.ID); err != nil {
		t.Fatal(err)
	}
	attached, err := database.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attached.SourceCredentialID != credential.ID || attached.SourceType != "git-private" {
		t.Fatalf("credential not attached: %#v", attached)
	}

	// Detaching returns the project to a public clone rather than leaving it
	// pointing at a credential that no longer exists.
	if err := database.SetProjectSourceCredential(ctx, project.ID, ""); err != nil {
		t.Fatal(err)
	}
	detached, _ := database.GetProject(ctx, project.ID)
	if detached.SourceCredentialID != "" || detached.SourceType != "git" {
		t.Fatalf("credential not detached: %#v", detached)
	}

	if err := database.SetProjectSourceCredential(ctx, project.ID, "no-such-credential"); err == nil {
		t.Fatal("expected an unknown credential to be rejected")
	}
}

// An image-imported project has nothing to clone, so attaching a Git credential
// to it is a mistake worth refusing rather than silently recording.
func TestSetProjectSourceCredentialRejectsNonGitProjects(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	credential := storeCredential(t, database, "github-https", "token")
	project := Project{ID: "project-2", Slug: "image", Name: "Image", SourceType: "image", SourcePath: "/tmp/image", ComposePath: "compose.yaml"}
	if err := database.CreateProject(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.SetProjectSourceCredential(ctx, project.ID, credential.ID); err == nil {
		t.Fatal("expected a non-Git project to be rejected")
	}
}
