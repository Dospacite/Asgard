package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rousoftware/asgard/internal/store"
	"gopkg.in/yaml.v3"
)

type Generator struct {
	Store *store.Store
	Dir   string
	// Domain is the control plane's own wildcard zone. Hostnames inside it get
	// the strong HSTS defaults; anything else has to opt in. Leaving it empty
	// means no name is treated as in-zone, which is the safe direction to fail.
	Domain string
}

type config struct {
	HTTP httpConfig `yaml:"http"`
}
type httpConfig struct {
	Routers     map[string]router     `yaml:"routers"`
	Services    map[string]service    `yaml:"services"`
	Middlewares map[string]middleware `yaml:"middlewares"`
}
type router struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
	Middlewares []string `yaml:"middlewares,omitempty"`
	TLS         tls      `yaml:"tls"`
}
type tls struct {
	CertResolver string `yaml:"certResolver"`
}
type service struct {
	LoadBalancer loadBalancer `yaml:"loadBalancer"`
}
type loadBalancer struct {
	Servers        []server     `yaml:"servers"`
	PassHostHeader bool         `yaml:"passHostHeader"`
	HealthCheck    *healthCheck `yaml:"healthCheck,omitempty"`
}
type server struct {
	URL string `yaml:"url"`
}
type healthCheck struct {
	Path     string `yaml:"path"`
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
}
type middleware struct {
	Headers headers `yaml:"headers"`
}

// hstsYear is the max-age every policy that sends the header at all uses.
const hstsYear = 31536000

// baseHeaders are the header protections that are safe on any hostname:
// they constrain how this response is treated, and make no claim about other
// names.
func baseHeaders() headers {
	return headers{FrameDeny: true, ContentTypeNosniff: true, ReferrerPolicy: "strict-origin-when-cross-origin"}
}

// securityMiddlewares builds one middleware per HSTS policy. Traefik dedupes
// by name, so emitting all four costs nothing and keeps the generated file
// readable next to the routers that reference them.
func securityMiddlewares() map[string]middleware {
	strict := baseHeaders()
	strict.STSSeconds, strict.STSIncludeSubdomains, strict.STSPreload = hstsYear, true, true
	standard := baseHeaders()
	standard.STSSeconds = hstsYear
	return map[string]middleware{
		middlewareName(store.HSTSStrict):   {Headers: strict},
		middlewareName(store.HSTSStandard): {Headers: standard},
		middlewareName(store.HSTSOff):      {Headers: baseHeaders()},
	}
}

// middlewareName maps a policy to its Traefik middleware.
//
// None of these may be called "asgard-security". Traefik's file provider shares
// one middleware namespace across every file in the directory, and the control
// plane's own control-plane.yml already defines "asgard-security" with the
// strong header for its own hostname. A generated file redefining that name
// with different contents makes which definition wins non-deterministic, and
// hand-written custom-domain files in the same directory reference it by name.
func middlewareName(policy string) string {
	switch policy {
	case store.HSTSStrict:
		return "asgard-hsts-strict"
	case store.HSTSOff:
		return "asgard-hsts-off"
	default:
		return "asgard-hsts"
	}
}

type headers struct {
	FrameDeny            bool   `yaml:"frameDeny"`
	ContentTypeNosniff   bool   `yaml:"contentTypeNosniff"`
	ReferrerPolicy       string `yaml:"referrerPolicy"`
	STSSeconds           int    `yaml:"stsSeconds"`
	STSIncludeSubdomains bool   `yaml:"stsIncludeSubdomains"`
	STSPreload           bool   `yaml:"stsPreload"`
}

var safeName = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

func (g *Generator) Write(ctx context.Context) error {
	rows, err := g.Store.DB.QueryContext(ctx, `SELECT r.id,r.hostname,r.target_port,s.health_path,s.hsts_mode,rc.docker_name FROM routes r JOIN services s ON s.id=r.service_id JOIN runtime_containers rc ON rc.service_id=r.service_id AND rc.active=1 WHERE r.tls=1 ORDER BY r.hostname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cfg := config{HTTP: httpConfig{Routers: map[string]router{}, Services: map[string]service{}, Middlewares: securityMiddlewares()}}
	for rows.Next() {
		var id, hostname, healthPath, hstsMode, containerName string
		var port int
		if err := rows.Scan(&id, &hostname, &port, &healthPath, &hstsMode, &containerName); err != nil {
			return err
		}
		name := strings.Trim(safeName.ReplaceAllString(id, "-"), "-")
		serviceName := "svc-" + name
		security := middlewareName(resolveHSTS(hstsMode, hostname, g.Domain))
		cfg.HTTP.Routers["route-"+name] = router{Rule: fmt.Sprintf("Host(`%s`)", hostname), EntryPoints: []string{"websecure"}, Service: serviceName, Middlewares: []string{security}, TLS: tls{CertResolver: "letsencrypt"}}
		check := &healthCheck{Path: healthPath, Interval: "15s", Timeout: "3s"}
		cfg.HTTP.Services[serviceName] = service{LoadBalancer: loadBalancer{Servers: []server{{URL: fmt.Sprintf("http://%s:%d", containerName, port)}}, PassHostHeader: true, HealthCheck: check}}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	bytes, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(g.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(g.Dir, "routes-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(bytes); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(g.Dir, "asgard-routes.yml"))
}

func (g *Generator) Hostnames(ctx context.Context) ([]string, error) {
	rows, err := g.Store.DB.QueryContext(ctx, `SELECT hostname FROM routes ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, rows.Err()
}
