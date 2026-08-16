package composecfg

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// variablePattern matches the Compose interpolation forms: $NAME, ${NAME},
// ${NAME:-default}, ${NAME-default}, ${NAME:+alt}, ${NAME+alt}, ${NAME:?err},
// ${NAME?err}, and the $$ escape for a literal dollar sign.
var variablePattern = regexp.MustCompile(`\$(\$|[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*(?:[:]?[-+?][^{}]*)?\})`)

// interpolationValues loads the variables Compose would interpolate from. Only
// the project's own .env file is consulted: the control plane's process
// environment belongs to Asgard, not to the workload, so leaking it into an
// imported Compose file would be both surprising and unsafe.
func interpolationValues(root string) map[string]string {
	if root == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		return nil
	}
	values, err := ParseEnvFile(data)
	if err != nil {
		return nil
	}
	return values
}

// interpolateNode substitutes variables in every scalar of a decoded YAML tree.
// Working on the tree rather than the raw bytes means a substituted value can
// never change the document's structure.
func interpolateNode(node *yaml.Node, values map[string]string, missing map[string]bool) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode && node.Tag != "!!null" {
		replaced, err := interpolate(node.Value, values, missing)
		if err != nil {
			return err
		}
		if replaced != node.Value {
			node.Value = replaced
			// A substituted scalar is text; clearing an inferred numeric or
			// boolean tag keeps "${PORT:-3000}" from decoding as an integer
			// when the surrounding field expects a string.
			if node.Tag == "!!int" || node.Tag == "!!bool" || node.Tag == "!!float" {
				node.Tag = "!!str"
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := interpolateNode(child, values, missing); err != nil {
			return err
		}
	}
	return nil
}

func interpolate(value string, values map[string]string, missing map[string]bool) (string, error) {
	var failure error
	result := variablePattern.ReplaceAllStringFunc(value, func(match string) string {
		body := match[1:]
		if body == "$" {
			return "$"
		}
		if !strings.HasPrefix(body, "{") {
			resolved, ok := values[body]
			if !ok {
				missing[body] = true
			}
			return resolved
		}
		body = strings.TrimSuffix(strings.TrimPrefix(body, "{"), "}")
		name, operator, argument := splitVariable(body)
		resolved, present := values[name]
		switch operator {
		case ":-":
			if resolved == "" {
				return argument
			}
		case "-":
			if !present {
				return argument
			}
		case ":+":
			if resolved != "" {
				return argument
			}
			return ""
		case "+":
			if present {
				return argument
			}
			return ""
		case ":?":
			if resolved == "" {
				failure = fmt.Errorf("required variable %s is unset or empty: %s", name, argument)
				return ""
			}
		case "?":
			if !present {
				failure = fmt.Errorf("required variable %s is unset: %s", name, argument)
				return ""
			}
		default:
			if !present {
				missing[name] = true
			}
		}
		return resolved
	})
	return result, failure
}

func splitVariable(body string) (name, operator, argument string) {
	for _, candidate := range []string{":-", ":+", ":?", "-", "+", "?"} {
		if index := strings.Index(body, candidate); index > 0 {
			return body[:index], candidate, body[index+len(candidate):]
		}
	}
	return body, "", ""
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
