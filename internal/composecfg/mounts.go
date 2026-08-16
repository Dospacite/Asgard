package composecfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectMountPrefix marks a volume spec whose source is a path inside the
// project's own imported source tree rather than a named Docker volume. A
// named volume can never begin with "@", so the two forms stay unambiguous
// everywhere a spec is split on ":".
const ProjectMountPrefix = "@project/"

// ProjectMount reports whether spec mounts a project-relative path and returns
// that path relative to the project root.
func ProjectMount(spec string) (string, bool) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || !strings.HasPrefix(parts[0], ProjectMountPrefix) {
		return "", false
	}
	return strings.TrimPrefix(parts[0], ProjectMountPrefix), true
}

// isBindSource reports whether a Compose volume source names a path rather
// than a named volume, following Compose's own rule: anything containing a
// path separator or beginning with a relative marker is a path.
func isBindSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") ||
		source == "." || source == ".." || strings.Contains(source, "/")
}

// normalizeProjectMount validates a project-relative bind source and rewrites
// it into the canonical @project/ form. Absolute host paths and any path that
// escapes the project root stay rejected: only files the project itself
// shipped can be mounted, which keeps the host filesystem out of reach while
// letting ordinary Compose files that mount their own config and secret files
// import unchanged.
func normalizeProjectMount(source, root string) (string, error) {
	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~") {
		return "", errors.New("host bind mounts are not allowed; use a named volume or a path inside the project")
	}
	if strings.Contains(source, `\`) {
		return "", errors.New("volume source must use forward slashes")
	}
	clean := filepath.ToSlash(filepath.Clean(source))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("volume source must stay inside the project directory")
	}
	if clean == "." {
		clean = ""
	}
	// The path is only checked against the filesystem when a root is known;
	// Compose previews and contract validation run without one.
	if root != "" {
		target := filepath.Join(root, filepath.FromSlash(clean))
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return "", err
		}
		if absTarget != absRoot && !strings.HasPrefix(absTarget, absRoot+string(filepath.Separator)) {
			return "", errors.New("volume source must stay inside the project directory")
		}
		if _, err := os.Lstat(absTarget); err != nil {
			return "", fmt.Errorf("%s is not present in the project source", clean)
		}
	}
	return ProjectMountPrefix + clean, nil
}
