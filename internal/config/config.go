package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr        string
	PublicURL         string
	Domain            string
	DataDir           string
	DatabasePath      string
	ProjectsDir       string
	BackupsDir        string
	TraefikDynamicDir string
	DockerHost        string
	EdgeNetwork       string
	ManagementNetwork string
	SecureCookies     bool
	AccessTTL         time.Duration
	RefreshTTL        time.Duration
	OperationWorkers  int
	MetricsInterval   time.Duration
	Timezone          *time.Location
	ACMEEmail         string
	DataVolume        string
	HelperImage       string
}

func Load() (Config, error) {
	dataDir := env("ASGARD_DATA_DIR", "/data")
	tzName := env("ASGARD_TIMEZONE", "Europe/Istanbul")
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		return Config{}, fmt.Errorf("load timezone %q: %w", tzName, err)
	}
	publicURL := strings.TrimRight(env("ASGARD_PUBLIC_URL", "https://asgard.rousoftware.com"), "/")
	domain := strings.TrimSpace(env("ASGARD_DOMAIN", "asgard.rousoftware.com"))
	if publicURL == "" || domain == "" {
		return Config{}, errors.New("ASGARD_PUBLIC_URL and ASGARD_DOMAIN are required")
	}
	workers, err := envInt("ASGARD_OPERATION_WORKERS", 1)
	if err != nil || workers < 1 || workers > 4 {
		return Config{}, errors.New("ASGARD_OPERATION_WORKERS must be between 1 and 4")
	}
	secure := !strings.EqualFold(env("ASGARD_SECURE_COOKIES", "true"), "false")
	return Config{
		ListenAddr:        env("ASGARD_LISTEN_ADDR", ":8080"),
		PublicURL:         publicURL,
		Domain:            domain,
		DataDir:           dataDir,
		DatabasePath:      env("ASGARD_DATABASE_PATH", filepath.Join(dataDir, "asgard.db")),
		ProjectsDir:       env("ASGARD_PROJECTS_DIR", filepath.Join(dataDir, "projects")),
		BackupsDir:        env("ASGARD_BACKUPS_DIR", filepath.Join(dataDir, "backups")),
		TraefikDynamicDir: env("ASGARD_TRAEFIK_DYNAMIC_DIR", filepath.Join(dataDir, "traefik", "dynamic")),
		DockerHost:        env("DOCKER_HOST", "unix:///var/run/docker.sock"),
		EdgeNetwork:       env("ASGARD_EDGE_NETWORK", "asgard-edge"),
		ManagementNetwork: env("ASGARD_MANAGEMENT_NETWORK", "asgard-management"),
		SecureCookies:     secure,
		AccessTTL:         15 * time.Minute,
		RefreshTTL:        30 * 24 * time.Hour,
		OperationWorkers:  workers,
		MetricsInterval:   30 * time.Second,
		Timezone:          tz,
		ACMEEmail:         strings.TrimSpace(os.Getenv("ASGARD_ACME_EMAIL")),
		DataVolume:        env("ASGARD_DATA_VOLUME", "asgard-data"),
		HelperImage:       env("ASGARD_HELPER_IMAGE", "asgard-control-plane:local"),
	}, nil
}

func (c Config) EnsureDirs() error {
	for _, dir := range []string{c.DataDir, c.ProjectsDir, c.BackupsDir, c.TraefikDynamicDir, filepath.Join(c.DataDir, "keys"), filepath.Join(c.DataDir, "uploads")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return fallback, nil
	}
	return strconv.Atoi(os.Getenv(name))
}
