package composecfg

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// loadEnvFiles reads the referenced files and returns their merged variables.
// Later files override earlier ones, matching Compose.
//
// Compose resolves an env_file relative to the directory holding the Compose
// file, so `deploy/compose.yaml` with `env_file: [.env]` reads deploy/.env.
// Keeping that rule here is what lets a project whose Compose file sits in a
// subdirectory import unchanged.
func loadEnvFiles(refs []EnvFileRef, source Source) (map[string]string, error) {
	merged := map[string]string{}
	for _, ref := range refs {
		clean := filepath.Clean(filepath.FromSlash(ref.Path))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("env_file %q must stay inside the project directory", ref.Path)
		}
		target := filepath.Join(source.AbsoluteDir(), clean)
		if err := requireInsideRoot(source.Root, filepath.ToSlash(filepath.Join(source.Directory(), clean))); err != nil {
			return nil, fmt.Errorf("env_file %q must stay inside the project directory", ref.Path)
		}
		data, err := os.ReadFile(target)
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
// and "#" comments are ignored, an optional "export " prefix is dropped, a
// single-quoted value is literal, a double-quoted value has its escapes
// decoded, and an unquoted value ends at a " #" comment.
//
// The source editor validates .env files with this same parser, so the values
// it shows are the values containers receive.
func ParseEnvFile(data []byte) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64<<10), MaxEnvironmentBytes)
	line := 0
	for scanner.Scan() {
		line++
		key, value, ok, err := ParseEnvLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if ok {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// ParseEnvLine parses one line of an env file. ok is false for a blank line or
// a comment.
func ParseEnvLine(raw string) (key, value string, ok bool, err error) {
	text := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
	if text == "" || strings.HasPrefix(text, "#") {
		return "", "", false, nil
	}
	if strings.HasPrefix(text, "export ") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "export "))
	}
	key, rawValue, found := strings.Cut(text, "=")
	key = strings.TrimSpace(key)
	if !found || !environmentVariablePattern.MatchString(key) {
		return "", "", false, errors.New("use KEY=VALUE with a variable name made of letters, numbers, and underscores")
	}
	value, err = ParseEnvValue(strings.TrimSpace(rawValue))
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

// ParseEnvValue decodes the right-hand side of a KEY=VALUE line.
func ParseEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.Index(raw[1:], "'")
		if end < 0 {
			return "", errors.New("single-quoted value is missing its closing quote")
		}
		end++
		if err := onlyComment(raw[end+1:]); err != nil {
			return "", err
		}
		return raw[1:end], nil
	case '"':
		end := closingDoubleQuote(raw)
		if end < 0 {
			return "", errors.New("double-quoted value is missing its closing quote")
		}
		if err := onlyComment(raw[end+1:]); err != nil {
			return "", err
		}
		value, err := strconv.Unquote(raw[:end+1])
		if err != nil {
			return "", errors.New("double-quoted value contains an invalid escape sequence")
		}
		return value, nil
	}
	for index, r := range raw {
		if r == '#' && index > 0 && (raw[index-1] == ' ' || raw[index-1] == '\t') {
			return strings.TrimSpace(raw[:index]), nil
		}
	}
	return raw, nil
}

func onlyComment(trailing string) error {
	trailing = strings.TrimSpace(trailing)
	if trailing != "" && !strings.HasPrefix(trailing, "#") {
		return errors.New("unexpected text after the quoted value")
	}
	return nil
}

func closingDoubleQuote(raw string) int {
	escaped := false
	for index := 1; index < len(raw); index++ {
		if escaped {
			escaped = false
			continue
		}
		if raw[index] == '\\' {
			escaped = true
			continue
		}
		if raw[index] == '"' {
			return index
		}
	}
	return -1
}

// ValidEnvironmentName reports whether name can be an environment variable.
func ValidEnvironmentName(name string) bool {
	return environmentVariablePattern.MatchString(name)
}
