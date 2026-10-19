package deploy

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rousoftware/asgard/internal/store"
	"golang.org/x/sync/errgroup"
)

// buildFunc builds one image and applies every tag in tags.
type buildFunc func(ctx context.Context, contextDir, dockerfile string, tags []string, target string, buildArgs map[string]string, log func(string)) error

// imageBuild is one image to build and the services that will run it.
type imageBuild struct {
	contextDir string
	dockerfile string
	target     string
	args       map[string]string
	services   []string
	tags       []string
}

// planBuilds groups the services that build from source by what they build.
// Services with the same context, Dockerfile, target and build arguments
// produce the same image, so it is built once and tagged for each of them.
// A worker that reuses the backend's Dockerfile costs nothing extra.
func planBuilds(sourcePath, slug string, version int, services []store.Service) ([]*imageBuild, map[string]string) {
	refs := map[string]string{}
	byKey := map[string]*imageBuild{}
	plan := []*imageBuild{}
	for _, svc := range services {
		if svc.BuildContext == "" {
			continue
		}
		contextDir := filepath.Join(sourcePath, svc.BuildContext)
		key := buildKey(contextDir, svc.Dockerfile, svc.Container.BuildTarget, svc.Container.BuildArgs)
		item, ok := byKey[key]
		if !ok {
			item = &imageBuild{contextDir: contextDir, dockerfile: svc.Dockerfile, target: svc.Container.BuildTarget, args: svc.Container.BuildArgs}
			byKey[key] = item
			plan = append(plan, item)
		}
		tag := fmt.Sprintf("asgard/%s/%s:r%d", slug, svc.Name, version)
		item.services = append(item.services, svc.Name)
		item.tags = append(item.tags, tag)
		refs[svc.ID] = tag
	}
	return plan, refs
}

func buildKey(contextDir, dockerfile, target string, args map[string]string) string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%q %q %q", filepath.Clean(contextDir), filepath.Clean(dockerfile), target)
	for _, key := range keys {
		fmt.Fprintf(&b, " %q=%q", key, args[key])
	}
	return b.String()
}

// runBuilds builds every planned image, up to concurrency at a time. The first
// failure cancels the builds still running. When more than one image builds,
// each log line is prefixed with the services it is for, since their output
// interleaves.
func runBuilds(ctx context.Context, plan []*imageBuild, concurrency int, build buildFunc, log func(string)) error {
	if concurrency < 1 {
		concurrency = 1
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(concurrency)
	for _, item := range plan {
		name := strings.Join(item.services, ", ")
		buildLog := log
		if len(plan) > 1 {
			buildLog = func(line string) { log("[" + name + "] " + line) }
		}
		group.Go(func() error {
			if len(item.services) > 1 {
				buildLog("Building one image for " + name + ", which share a build")
			}
			if err := build(ctx, item.contextDir, item.dockerfile, item.tags, item.target, item.args, buildLog); err != nil {
				return fmt.Errorf("build %s: %w", name, err)
			}
			return nil
		})
	}
	return group.Wait()
}
