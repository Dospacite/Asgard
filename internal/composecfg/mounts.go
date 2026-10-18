package composecfg

import (
	"errors"
	"fmt"
	"os"
	"path"
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

// VolumeRequest is one parsed Compose volume entry, before Asgard scopes it.
//
// The short form (`./config/x.yml:/etc/x.yml:ro`) and the long form
// (`{type: bind, source: ./config, target: /etc}`) both land here, so the rest
// of the layer only ever sees one shape.
type VolumeRequest struct {
	// Source is the declared source: a named volume, a project-relative path,
	// or empty for an anonymous volume.
	Source string
	// Target is the absolute path inside the container.
	Target string
	// ReadOnly carries the :ro mode or read_only: true.
	ReadOnly bool
	// Project marks a source that is a path inside the project's own source
	// tree rather than a named Docker volume.
	Project bool
}

// mount modes Compose accepts after the second colon. Only "ro" changes what
// Asgard does; the rest are SELinux relabel requests and mount-propagation
// hints, which are meaningless for a workload that only ever sees its own
// project volumes. Accepting them is what lets an ordinary Compose file — which
// on a SELinux host carries `:z` on nearly every mount — import unchanged.
var mountModes = map[string]bool{
	"ro": true, "rw": true,
	"z": true, "Z": true,
	"cached": true, "delegated": true, "consistent": true,
	"nocopy": true,
	"shared": true, "slave": true, "private": true,
	"rshared": true, "rslave": true, "rprivate": true,
}

// volumeFields classifies the keys of a long-syntax volume entry.
var volumeFields = newFieldTable(
	Field{Name: "type", Policy: Applied},
	Field{Name: "source", Policy: Applied},
	Field{Name: "target", Policy: Applied},
	Field{Name: "read_only", Policy: Applied},
	Field{Name: "consistency", Policy: Ignored, Note: "Consistency hints only affect Docker Desktop's file sharing."},
	Field{Name: "bind", Policy: Ignored, Note: "Bind propagation is not configurable; the mount is served from the project's own source."},
	Field{Name: "volume", Policy: Ignored, Note: "Volume sub-paths and nocopy are not applied."},
	Field{Name: "tmpfs", Policy: Ignored, Note: "Use the service-level `tmpfs` key instead; it is applied."},
)

// parseVolume accepts every form Compose allows for one service volume entry.
// A long-syntax entry is a mapping of its own, so its keys are classified here
// — an ignored option inside a volume entry is reported exactly like an ignored
// option on a service.
func parseVolume(value any, result *ValidationResult, path string) (VolumeRequest, error) {
	switch entry := value.(type) {
	case string:
		return parseShortVolume(entry)
	case map[string]any:
		classify(entry, volumeFields, path, result)
		return parseLongVolume(entry)
	default:
		return VolumeRequest{}, errors.New("volume must be a string or a mapping")
	}
}

func parseShortVolume(spec string) (VolumeRequest, error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return VolumeRequest{}, errors.New("volume must be [source:]target[:mode], for example ./config.yml:/app/config.yml:ro")
	}
	request := VolumeRequest{}
	switch len(parts) {
	case 1:
		request.Target = parts[0]
	case 2:
		request.Source, request.Target = parts[0], parts[1]
	default:
		request.Source, request.Target = parts[0], parts[1]
		for _, mode := range strings.Split(parts[2], ",") {
			mode = strings.TrimSpace(mode)
			if mode == "" {
				continue
			}
			if !mountModes[mode] {
				return VolumeRequest{}, fmt.Errorf("%q is not a supported volume mode", mode)
			}
			if mode == "ro" {
				request.ReadOnly = true
			}
		}
	}
	if !strings.HasPrefix(request.Target, "/") {
		return VolumeRequest{}, errors.New("container mount path must be absolute")
	}
	if request.Source == "" {
		// An anonymous volume: Compose gives the container a fresh volume at
		// that path. Asgard gives it a stable one instead, so the data a
		// release writes survives the next release.
		return request, nil
	}
	if request.Source == "." || request.Source == ".." {
		return VolumeRequest{}, errors.New("volume source must name a path or a declared volume")
	}
	request.Project = isBindSource(request.Source)
	if strings.HasPrefix(request.Source, "~") {
		return VolumeRequest{}, errors.New("host bind mounts are not allowed; use a named volume or a path inside the project")
	}
	return request, nil
}

func parseLongVolume(entry map[string]any) (VolumeRequest, error) {
	request := VolumeRequest{}
	for key, raw := range entry {
		switch key {
		case "type":
			switch fmt.Sprint(raw) {
			case "bind", "volume":
			case "tmpfs":
				return VolumeRequest{}, errors.New("a tmpfs volume is not supported here; declare the path under the service's own `tmpfs` key, which Asgard applies")
			default:
				return VolumeRequest{}, fmt.Errorf("volume type %q is not supported", fmt.Sprint(raw))
			}
		case "source":
			request.Source = fmt.Sprint(raw)
		case "target":
			request.Target = fmt.Sprint(raw)
		case "read_only":
			request.ReadOnly = fmt.Sprint(raw) == "true"
		}
	}
	if request.Target == "" || !strings.HasPrefix(request.Target, "/") {
		return VolumeRequest{}, errors.New("container mount path must be absolute")
	}
	if request.Source == "" {
		return request, nil
	}
	request.Project = isBindSource(request.Source)
	if filepath.IsAbs(request.Source) || strings.HasPrefix(request.Source, "~") {
		return VolumeRequest{}, errors.New("host bind mounts are not allowed; use a named volume or a path inside the project")
	}
	return request, nil
}

// isBindSource reports whether a Compose volume source names a path rather
// than a named volume, following Compose's own rule: anything containing a
// path separator or beginning with a relative marker is a path.
func isBindSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") ||
		source == "." || source == ".." || strings.Contains(source, "/")
}

// looksLikeFilePath reports whether a path the project does not ship was meant
// to be a file. A name with an extension, or a dotfile, has to exist in the
// source tree: mounting an empty directory over it would fail much later and
// much less clearly than saying so at import. Anything else reads as a data
// directory, which Compose creates on the host and Asgard creates in the
// project's own source tree.
func looksLikeFilePath(relative string) bool {
	name := path.Base(relative)
	return strings.HasPrefix(name, ".") || strings.Contains(name[1:], ".")
}

// normalizeProjectMount validates a project-relative bind source and rewrites
// it into the canonical @project/ form. Absolute host paths and any path that
// escapes the project root stay rejected: only files the project itself
// shipped can be mounted, which keeps the host filesystem out of reach while
// letting ordinary Compose files that mount their own config and secret files
// import unchanged.
//
// The path is resolved the way Compose resolves it — relative to the directory
// holding the Compose file, not to the repository root — so a file at
// deploy/compose.yaml that mounts `./nginx.conf` gets deploy/nginx.conf.
//
// Returns the path relative to the project root, expressed with forward
// slashes. root may be empty: contract previews run without a source tree, and
// then only the lexical checks apply.
func normalizeProjectMount(source, root, composeDir string) (string, error) {
	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~") {
		return "", errors.New("host bind mounts are not allowed; use a named volume or a path inside the project")
	}
	if strings.Contains(source, `\`) {
		return "", errors.New("volume source must use forward slashes")
	}
	clean := path.Join(composeDir, filepath.ToSlash(source))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("volume source must stay inside the project directory")
	}
	if clean == "." {
		clean = ""
	}
	if root != "" {
		if err := requireInsideRoot(root, clean); err != nil {
			return "", err
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(clean))); err != nil {
			if os.IsNotExist(err) && clean != "" && !looksLikeFilePath(clean) {
				// A data directory the project does not ship yet. The deployer
				// creates it in the project's own source tree before the
				// container starts, which is where Compose would have created
				// it on the host.
				return ProjectMountPrefix + clean, nil
			}
			return "", fmt.Errorf("%s is not present in the project source", displayPath(clean))
		}
	}
	return ProjectMountPrefix + clean, nil
}

// requireInsideRoot rejects a project-relative path that resolves outside the
// project, following symlinks on the way. It re-checks after resolution because
// the lexical form cannot see a symlinked parent directory.
func requireInsideRoot(root, relative string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(filepath.Join(absRoot, filepath.FromSlash(relative)))
	if err != nil {
		return err
	}
	if absTarget != absRoot && !strings.HasPrefix(absTarget, absRoot+string(filepath.Separator)) {
		return errors.New("volume source must stay inside the project directory")
	}
	return nil
}

func displayPath(relative string) string {
	if relative == "" {
		return "the project root"
	}
	return relative
}
