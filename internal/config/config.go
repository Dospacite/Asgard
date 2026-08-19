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
	// CredentialVerifyInterval is how often stored Git credentials are re-proven
	// against their repositories. Credentials rot without producing any event
	// Asgard would otherwise see.
	CredentialVerifyInterval time.Duration
	// Storage reclamation. Asgard tags an image per service per release and
	// caches every build layer, so without a retention policy a host fills up
	// on its own.
	KeepReleaseImages int
	BuildCacheBytes   int64
	ReclaimInterval   time.Duration
	Timezone          *time.Location
	ACMEEmail         string
	DataVolume        string
	HelperImage       string
}

func Load() (Config, error) {
	dataDir := env("ASGARD_DATA_DIR", "/data")
	tzName := env("ASGARD_TIMEZONE", "UTC")
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		return Config{}, fmt.Errorf("load timezone %q: %w", tzName, err)
	}
	// These have no defaults on purpose. Every install runs under its own
	// domain, and a built-in fallback pointing at someone else's would be wrong
	// in a way that is easy to miss: hostnames would be generated, certificates
	// requested, and HSTS zone rules decided against a zone this host does not
	// own. Failing at startup with a clear message is the honest alternative.
	publicURL := strings.TrimRight(strings.TrimSpace(os.Getenv("ASGARD_PUBLIC_URL")), "/")
	domain := strings.TrimSpace(os.Getenv("ASGARD_DOMAIN"))
	if publicURL == "" || domain == "" {
		return Config{}, errors.New("ASGARD_PUBLIC_URL and ASGARD_DOMAIN are required: set them in deploy/.env (see deploy/.env.example)")
	}
	workers, err := envInt("ASGARD_OPERATION_WORKERS", 1)
	if err != nil || workers < 1 || workers > 4 {
		return Config{}, errors.New("ASGARD_OPERATION_WORKERS must be between 1 and 4")
	}
	secure := !strings.EqualFold(env("ASGARD_SECURE_COOKIES", "true"), "false")
	verifyHours, err := envInt("ASGARD_CREDENTIAL_VERIFY_HOURS", 6)
	if err != nil || verifyHours < 0 || verifyHours > 168 {
		return Config{}, errors.New("ASGARD_CREDENTIAL_VERIFY_HOURS must be between 0 and 168")
	}
	verifyInterval := time.Duration(verifyHours) * time.Hour
	// How many releases keep their images. This is how far back a rollback can
	// reach, so the floor is 1 rather than 0.
	keepImages, err := envInt("ASGARD_KEEP_RELEASE_IMAGES", 3)
	if err != nil || keepImages < 1 || keepImages > 50 {
		return Config{}, errors.New("ASGARD_KEEP_RELEASE_IMAGES must be between 1 and 50")
	}
	cacheGB, err := envInt("ASGARD_BUILD_CACHE_GB", 2)
	if err != nil || cacheGB < 0 || cacheGB > 500 {
		return Config{}, errors.New("ASGARD_BUILD_CACHE_GB must be between 0 and 500")
	}
	reclaimHours, err := envInt("ASGARD_RECLAIM_HOURS", 12)
	if err != nil || reclaimHours < 0 || reclaimHours > 168 {
		return Config{}, errors.New("ASGARD_RECLAIM_HOURS must be between 0 and 168")
	}
	return Config{
		ListenAddr:               env("ASGARD_LISTEN_ADDR", ":8080"),
		PublicURL:                publicURL,
		Domain:                   domain,
		DataDir:                  dataDir,
		DatabasePath:             env("ASGARD_DATABASE_PATH", filepath.Join(dataDir, "asgard.db")),
		ProjectsDir:              env("ASGARD_PROJECTS_DIR", filepath.Join(dataDir, "projects")),
		BackupsDir:               env("ASGARD_BACKUPS_DIR", filepath.Join(dataDir, "backups")),
		TraefikDynamicDir:        env("ASGARD_TRAEFIK_DYNAMIC_DIR", filepath.Join(dataDir, "traefik", "dynamic")),
		DockerHost:               env("DOCKER_HOST", "unix:///var/run/docker.sock"),
		EdgeNetwork:              env("ASGARD_EDGE_NETWORK", "asgard-edge"),
		ManagementNetwork:        env("ASGARD_MANAGEMENT_NETWORK", "asgard-management"),
		SecureCookies:            secure,
		AccessTTL:                15 * time.Minute,
		RefreshTTL:               30 * 24 * time.Hour,
		OperationWorkers:         workers,
		MetricsInterval:          30 * time.Second,
		CredentialVerifyInterval: verifyInterval,
		KeepReleaseImages:        keepImages,
		BuildCacheBytes:          int64(cacheGB) << 30,
		ReclaimInterval:          time.Duration(reclaimHours) * time.Hour,
		Timezone:                 tz,
		ACMEEmail:                strings.TrimSpace(os.Getenv("ASGARD_ACME_EMAIL")),
		DataVolume:               env("ASGARD_DATA_VOLUME", "asgard-data"),
		HelperImage:              env("ASGARD_HELPER_IMAGE", "asgard-control-plane:local"),
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
