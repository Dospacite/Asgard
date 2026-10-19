package dockerx

import (
	"errors"
	"os"
	"path"
	"path/filepath"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// dockerignore holds the exclusion rules for one build context.
type dockerignore struct {
	matcher *patternmatcher.PatternMatcher
	// hasExceptions is set when a `!pattern` can re-include something below an
	// excluded directory, so the walk must still descend into it.
	hasExceptions bool
	// keep names the files the builder always needs, even when a pattern
	// matches them: the Dockerfile and the ignore file itself.
	keep map[string]bool
}

// loadDockerignore reads the ignore rules for a build the way BuildKit picks
// them: a `<Dockerfile>.dockerignore` beside the Dockerfile wins over the
// context's own `.dockerignore`. No file means nothing is excluded.
func loadDockerignore(root, dockerfile string) (*dockerignore, error) {
	dockerfile = path.Clean(filepath.ToSlash(dockerfile))
	rules := &dockerignore{keep: map[string]bool{dockerfile: true}}
	for _, name := range []string{dockerfile + ".dockerignore", ".dockerignore"} {
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(name)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		patterns, err := ignorefile.ReadAll(file)
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		matcher, err := patternmatcher.New(patterns)
		if err != nil {
			return nil, err
		}
		rules.matcher = matcher
		rules.hasExceptions = matcher.Exclusions()
		rules.keep[name] = true
		break
	}
	return rules, nil
}

// excludes reports whether a context-relative, slash-separated path is left
// out of the build context.
func (d *dockerignore) excludes(rel string, isDir bool) bool {
	if d == nil || d.matcher == nil || d.keep[rel] {
		return false
	}
	// A directory that holds a file the builder needs is never skipped whole.
	if isDir {
		for keep := range d.keep {
			if len(keep) > len(rel) && keep[:len(rel)+1] == rel+"/" {
				return false
			}
		}
	}
	excluded, _ := d.matcher.MatchesOrParentMatches(rel)
	return excluded
}
