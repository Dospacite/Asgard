package store

import (
	"context"
	"encoding/json"
)

// ComposedSettings are the operator-editable settings of a service as the
// Compose file produced them the last time the file was applied.
//
// A reconcile needs this to tell two cases apart: a stored value an operator
// set through Asgard, which must survive, and a stored value the file itself
// produced earlier, which the file may now change. Re-parsing the old Compose
// file cannot answer that, because env files and the project's .env are read
// from disk at parse time and may already hold the new values.
type ComposedSettings struct {
	Role          string            `json:"role"`
	Environment   map[string]string `json:"environment"`
	Public        bool              `json:"public"`
	Port          int               `json:"port"`
	Hostname      string            `json:"hostname"`
	HealthPath    string            `json:"healthPath"`
	RestartPolicy string            `json:"restartPolicy"`
	CPULimit      float64           `json:"cpuLimit"`
	MemoryLimit   int64             `json:"memoryLimit"`
	PIDsLimit     int64             `json:"pidsLimit"`
}

// ComposedFrom captures the settings a parsed service carries.
func ComposedFrom(svc Service) ComposedSettings {
	environment := make(map[string]string, len(svc.Environment))
	for key, value := range svc.Environment {
		environment[key] = value
	}
	return ComposedSettings{Role: svc.Role, Environment: environment, Public: svc.Public, Port: svc.Port, Hostname: svc.Hostname, HealthPath: svc.HealthPath, RestartPolicy: svc.RestartPolicy, CPULimit: svc.CPULimit, MemoryLimit: svc.MemoryLimit, PIDsLimit: svc.PIDsLimit}
}

// ComposedJSON is the column value recording what the file produced for svc.
func ComposedJSON(svc Service) string {
	data, _ := json.Marshal(ComposedFrom(svc))
	return string(data)
}

// Service returns the settings laid over a service, for code that compares
// whole services field by field.
func (c ComposedSettings) Service(name string) Service {
	return Service{Name: name, Role: c.Role, Environment: c.Environment, Public: c.Public, Port: c.Port, Hostname: c.Hostname, HealthPath: c.HealthPath, RestartPolicy: c.RestartPolicy, CPULimit: c.CPULimit, MemoryLimit: c.MemoryLimit, PIDsLimit: c.PIDsLimit}
}

// ComposedBaselines returns, by service name, what the Compose file last
// produced for each service of a project. A service with no record yet — one
// created before the record existed, or added outside a Compose file — is
// absent.
func (s *Store) ComposedBaselines(ctx context.Context, projectID string) (map[string]ComposedSettings, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT name,composed_json FROM services WHERE project_id=? AND composed_json<>''`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ComposedSettings{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var settings ComposedSettings
		if json.Unmarshal([]byte(raw), &settings) != nil {
			continue
		}
		if settings.Environment == nil {
			settings.Environment = map[string]string{}
		}
		out[name] = settings
	}
	return out, rows.Err()
}
