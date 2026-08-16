package composecfg

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvFileRef is one entry of a service's env_file, in either the short string
// form or Compose's long form with an explicit required flag.
type EnvFileRef struct {
	Path     string
	Required bool
}

// parseEnvFiles normalizes the several shapes Compose accepts for env_file:
// a single path, a list of paths, or a list of {path, required} mappings.
func parseEnvFiles(value any) ([]EnvFileRef, error) {
	if value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case string:
		return []EnvFileRef{{Path: v, Required: true}}, nil
	case []any:
		refs := make([]EnvFileRef, 0, len(v))
		for _, item := range v {
			switch entry := item.(type) {
			case string:
				refs = append(refs, EnvFileRef{Path: entry, Required: true})
			case map[string]any:
				ref := EnvFileRef{Required: true}
				for key, raw := range entry {
					switch key {
					case "path":
						ref.Path = fmt.Sprint(raw)
					case "required":
						ref.Required = fmt.Sprint(raw) != "false"
					default:
						return nil, fmt.Errorf("env_file.%s is not supported", key)
					}
				}
				if ref.Path == "" {
					return nil, errors.New("env_file entries need a path")
				}
				refs = append(refs, ref)
			default:
				return nil, errors.New("env_file entries must be paths or {path, required} mappings")
			}
		}
		return refs, nil
	default:
		return nil, errors.New("env_file must be a path or a list of paths")
	}
}

// loadEnvFiles reads the referenced files from the project root and returns
// their merged variables. Later files override earlier ones, matching Compose.
func loadEnvFiles(refs []EnvFileRef, root string) (map[string]string, error) {
	merged := map[string]string{}
	for _, ref := range refs {
		clean := filepath.Clean(filepath.FromSlash(ref.Path))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("env_file %q must stay inside the project directory", ref.Path)
		}
		data, err := os.ReadFile(filepath.Join(root, clean))
		if err != nil {
			if os.IsNotExist(err) && !ref.Required {
				continue
			}
			return nil, fmt.Errorf("read env_file %q: %w", ref.Path, err)
		}
		values, err := ParseEnvFile(data)
		if err != nil {
			return nil, fmt.Errorf("env_file %q: %w", ref.Path, err)
		}
		for key, value := range values {
			merged[key] = value
		}
	}
	return merged, nil
}

// ParseEnvFile reads the KEY=VALUE format Docker Compose accepts: blank lines
// and "#" comments are ignored, an optional "export " prefix is dropped, and a
// fully single- or double-quoted value is unquoted.
func ParseEnvFile(data []byte) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64<<10), MaxEnvironmentBytes)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("line %d is not KEY=VALUE", line)
		}
		key = strings.TrimSpace(key)
		if !environmentVariablePattern.MatchString(key) {
			return nil, fmt.Errorf("line %d has an invalid variable name %q", line, key)
		}
		values[key] = unquoteEnvValue(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func unquoteEnvValue(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first == last && (first == '"' || first == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
