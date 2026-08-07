package composecfg

var Contract = map[string]any{
	"version":       "1.0",
	"topLevel":      []string{"name", "version", "services", "volumes", "networks", "x-asgard"},
	"serviceFields": []string{"image", "build", "command", "environment", "ports", "expose", "volumes", "depends_on", "healthcheck", "restart"},
	"roles":         []string{"web", "worker", "stateful"},
	"defaults":      map[string]any{"cpu": 0.5, "memoryBytes": 512 << 20, "pids": 256, "healthPath": "/"},
	"rejected":      []string{"privileged", "network_mode", "pid", "ipc", "devices", "cap_add", "cap_drop", "security_opt", "extra_hosts", "userns_mode", "container_name", "host bind mounts", "Docker socket mounts", "Compose secrets/configs"},
	"extension": map[string]any{
		"x-asgard": map[string]any{"primary-service": "service name", "services": map[string]any{"<name>": map[string]any{"role": "web|worker|stateful", "public": "boolean", "port": "internal container port", "health-path": "HTTP path", "hostname": "optional hostname under the configured wildcard"}}},
	},
}
