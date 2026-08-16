package composecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func serviceNamed(t *testing.T, result ValidationResult, name string) int {
	t.Helper()
	for index, svc := range result.Services {
		if svc.Name == name {
			return index
		}
	}
	t.Fatalf("service %q missing from %d parsed services", name, len(result.Services))
	return -1
}

func TestParseMergesEnvFileUnderInlineEnvironment(t *testing.T) {
	root := writeProject(t, map[string]string{
		".env": "# comment\n\nexport NODE_ENV=production\nPORT=3000\nTOKEN_SECRET=\"quoted value\"\nEMPTY=\n",
		"compose.yaml": `services:
  api:
    image: nginx:1.27
    env_file:
      - .env
    environment:
      PORT: 8080
`,
	})
	data, err := os.ReadFile(filepath.Join(root, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, result := Parse(data, "project", "demo", root)
	if !result.Valid {
		t.Fatalf("expected valid compose, errors: %+v", result.Errors)
	}
	env := result.Services[serviceNamed(t, result, "api")].Environment
	if env["NODE_ENV"] != "production" {
		t.Fatalf("NODE_ENV = %q", env["NODE_ENV"])
	}
	if env["TOKEN_SECRET"] != "quoted value" {
		t.Fatalf("TOKEN_SECRET = %q", env["TOKEN_SECRET"])
	}
	if env["EMPTY"] != "" {
		t.Fatalf("EMPTY = %q", env["EMPTY"])
	}
	if env["PORT"] != "8080" {
		t.Fatalf("inline environment must win over env_file, got PORT = %q", env["PORT"])
	}
}

func TestParseOptionalEnvFileMayBeMissing(t *testing.T) {
	root := writeProject(t, map[string]string{"compose.yaml": `services:
  api:
    image: nginx:1.27
    env_file:
      - path: .env
        required: false
`})
	data, _ := os.ReadFile(filepath.Join(root, "compose.yaml"))
	if _, result := Parse(data, "project", "demo", root); !result.Valid {
		t.Fatalf("optional env_file should not fail validation: %+v", result.Errors)
	}
}

func TestParseRequiredEnvFileMustExist(t *testing.T) {
	root := writeProject(t, map[string]string{"compose.yaml": `services:
  api:
    image: nginx:1.27
    env_file: .env
`})
	data, _ := os.ReadFile(filepath.Join(root, "compose.yaml"))
	_, result := Parse(data, "project", "demo", root)
	if result.Valid {
		t.Fatal("a required env_file that is absent must fail validation")
	}
}

func TestParseAcceptsProjectRelativeMount(t *testing.T) {
	root := writeProject(t, map[string]string{
		"secrets/google.json": "{}",
		"compose.yaml": `services:
  api:
    image: nginx:1.27
    volumes:
      - ./secrets/google.json:/app/secrets/google-client-secret.json:ro
`,
	})
	data, _ := os.ReadFile(filepath.Join(root, "compose.yaml"))
	_, result := Parse(data, "project", "demo", root)
	if !result.Valid {
		t.Fatalf("project-relative mount rejected: %+v", result.Errors)
	}
	volumes := result.Services[serviceNamed(t, result, "api")].Volumes
	want := ProjectMountPrefix + "secrets/google.json:/app/secrets/google-client-secret.json:ro"
	if len(volumes) != 1 || volumes[0] != want {
		t.Fatalf("volumes = %v, want [%s]", volumes, want)
	}
	relative, ok := ProjectMount(volumes[0])
	if !ok || relative != "secrets/google.json" {
		t.Fatalf("ProjectMount = %q, %v", relative, ok)
	}
}

func TestParseRejectsHostAndEscapingMounts(t *testing.T) {
	for name, spec := range map[string]string{
		"absolute host path": "/etc/passwd:/app/passwd:ro",
		"docker socket":      "/var/run/docker.sock:/var/run/docker.sock",
		"parent escape":      "../../etc:/app/etc:ro",
		"home expansion":     "~/.ssh:/app/ssh:ro",
	} {
		root := writeProject(t, map[string]string{"compose.yaml": "services:\n  api:\n    image: nginx:1.27\n    volumes:\n      - " + spec + "\n"})
		data, _ := os.ReadFile(filepath.Join(root, "compose.yaml"))
		if _, result := Parse(data, "project", "demo", root); result.Valid {
			t.Fatalf("%s (%q) was accepted", name, spec)
		}
	}
}

func TestParseRejectsMountOfMissingProjectPath(t *testing.T) {
	root := writeProject(t, map[string]string{"compose.yaml": `services:
  api:
    image: nginx:1.27
    volumes:
      - ./nowhere.json:/app/nowhere.json:ro
`})
	data, _ := os.ReadFile(filepath.Join(root, "compose.yaml"))
	_, result := Parse(data, "project", "demo", root)
	if result.Valid {
		t.Fatal("a mount of a path the project does not ship must fail validation")
	}
}

func TestParseStillScopesNamedVolumes(t *testing.T) {
	root := writeProject(t, map[string]string{"compose.yaml": `volumes:
  postgres_data: {}
services:
  db:
    image: postgres:16-alpine
    volumes:
      - postgres_data:/var/lib/postgresql/data
`})
	data, _ := os.ReadFile(filepath.Join(root, "compose.yaml"))
	_, result := Parse(data, "project", "demo", root)
	if !result.Valid {
		t.Fatalf("named volume rejected: %+v", result.Errors)
	}
	volumes := result.Services[serviceNamed(t, result, "db")].Volumes
	if len(volumes) != 1 || volumes[0] != "asgard-demo-postgres-data:/var/lib/postgresql/data" {
		t.Fatalf("volumes = %v", volumes)
	}
	if _, ok := ProjectMount(volumes[0]); ok {
		t.Fatal("named volume misidentified as a project mount")
	}
}

func TestValidateGitSource(t *testing.T) {
	if _, err := ValidateGitSource("https://github.com/owner/repo.git", false); err != nil {
		t.Fatalf("public HTTPS rejected: %v", err)
	}
	if _, err := ValidateGitSource("https://token@github.com/owner/repo.git", false); err == nil {
		t.Fatal("URL-embedded credentials accepted")
	}
	if _, err := ValidateGitSource("git@github.com:owner/repo.git", false); err == nil {
		t.Fatal("scp-style URL accepted without an SSH credential")
	}
	source, err := ValidateGitSource("git@github.com:owner/repo.git", true)
	if err != nil {
		t.Fatalf("scp-style URL rejected with an SSH credential: %v", err)
	}
	if source.Scheme != "ssh" || source.Host != "github.com" {
		t.Fatalf("source = %+v", source)
	}
	if _, err := ValidateGitSource("http://github.com/owner/repo.git", false); err == nil {
		t.Fatal("plaintext HTTP accepted")
	}
	if _, err := ValidateGitSource("https://localhost/repo.git", false); err == nil {
		t.Fatal("loopback host accepted")
	}
}

func TestParseEnvFileRejectsMalformedLines(t *testing.T) {
	if _, err := ParseEnvFile([]byte("NOT_A_PAIR\n")); err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ParseEnvFile([]byte("1BAD=value\n")); err == nil {
		t.Fatal("invalid variable name accepted")
	}
}
