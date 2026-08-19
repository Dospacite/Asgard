// Package reclaim decides which Docker artifacts Asgard may delete.
//
// Asgard tags every build `asgard/<slug>/<service>:r<version>` and never
// removed any of them, so a host accumulated one image per service per release
// forever. Deleting a project removed its containers, network, and source but
// left every image it had ever built. Build cache had no bound at all. The
// result was a control plane that needed manual `docker system prune` to keep
// running — maintenance the product caused and did not perform.
//
// The policy here is deliberately conservative. Reclaiming disk must never
// break a rollback, so the images backing recent releases are kept, and an
// image any container still references is never touched.
package reclaim

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/store"
)

// releaseTag matches the tags Asgard's own builds produce. Anything that does
// not match — a pulled upstream image, an operator's own build — is not
// Asgard's to delete and is never considered.
var releaseTag = regexp.MustCompile(`^asgard/([a-z0-9][a-z0-9-]*)/([a-zA-Z0-9._-]+):r(\d+)$`)

// Policy bounds what is kept.
type Policy struct {
	// KeepReleases is how many of a project's most recent releases keep their
	// images. This is exactly how far back a rollback can still reach, so it is
	// a durability setting, not just a disk one.
	KeepReleases int
	// BuildCacheBytes caps the builder cache. Zero disables cache pruning.
	BuildCacheBytes int64
	// DryRun reports what would be removed without removing it.
	DryRun bool
}

// Item is one artifact considered for removal.
type Item struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	Reason    string `json:"reason"`
	SizeBytes int64  `json:"sizeBytes"`
	Removed   bool   `json:"removed"`
	Error     string `json:"error,omitempty"`
}

// Result summarizes one pass.
type Result struct {
	Items         []Item `json:"items"`
	ImagesRemoved int    `json:"imagesRemoved"`
	// ApparentBytes is the sum of the removed images' individual sizes. It
	// double-counts every layer they share, and release images of one service
	// share nearly all of theirs, so it overstates — often by more than half.
	// It is reported only because a dry run has nothing better to offer.
	ApparentBytes int64 `json:"apparentBytes"`
	// FreedBytes is the daemon's own before-and-after measurement: the number
	// that matches what `df` will show. Zero on a dry run.
	FreedBytes      int64 `json:"freedBytes"`
	BuildCacheBytes int64 `json:"buildCacheReclaimedBytes"`
	DryRun          bool  `json:"dryRun"`
	KeptForRollback int   `json:"keptForRollback"`
	Errors          int   `json:"errors"`
}

// Reclaimer frees disk that Asgard itself allocated.
type Reclaimer struct {
	Store  *store.Store
	Docker *dockerx.Engine
	Policy Policy
	Logger *slog.Logger
}

func (r *Reclaimer) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Reclaimer) keepReleases() int {
	if r.Policy.KeepReleases < 1 {
		return 1
	}
	return r.Policy.KeepReleases
}

// parsedTag is a release image tag broken into its parts.
type parsedTag struct {
	slug    string
	service string
	version int
}

func parseReleaseTag(tag string) (parsedTag, bool) {
	match := releaseTag.FindStringSubmatch(tag)
	if match == nil {
		return parsedTag{}, false
	}
	version, err := strconv.Atoi(match[3])
	if err != nil {
		return parsedTag{}, false
	}
	return parsedTag{slug: match[1], service: match[2], version: version}, true
}

// keepSet is the set of (slug, version) pairs whose images must survive.
type keepSet map[string]map[int]bool

func (k keepSet) has(slug string, version int) bool {
	versions, ok := k[slug]
	return ok && versions[version]
}

func (k keepSet) knownSlug(slug string) bool {
	_, ok := k[slug]
	return ok
}

// protectedVersions computes, per project slug, the release versions whose
// images must be kept: the newest KeepReleases releases plus whatever any
// active container is actually running. A project with no releases yet still
// appears, so its slug is not mistaken for an orphan.
func (r *Reclaimer) protectedVersions(ctx context.Context) (keepSet, error) {
	keep := keepSet{}
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT id,slug FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type project struct{ id, slug string }
	projects := []project{}
	for rows.Next() {
		var p project
		if err := rows.Scan(&p.id, &p.slug); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range projects {
		keep[p.slug] = map[int]bool{}
		// The newest releases, whatever their status. A failed release still
		// holds the images a retry would reuse, and counting only successful
		// ones would quietly discard the version an operator is debugging.
		versionRows, err := r.Store.DB.QueryContext(ctx, `SELECT version FROM releases WHERE project_id=? ORDER BY version DESC LIMIT ?`, p.id, r.keepReleases())
		if err != nil {
			return nil, err
		}
		for versionRows.Next() {
			var version int
			if err := versionRows.Scan(&version); err != nil {
				versionRows.Close()
				return nil, err
			}
			keep[p.slug][version] = true
		}
		versionRows.Close()
		// Anything currently running is protected regardless of age. A project
		// that has not been redeployed in a long time must never have the
		// image out from under its running container.
		activeRows, err := r.Store.DB.QueryContext(ctx, `SELECT DISTINCT rel.version FROM runtime_containers rc JOIN releases rel ON rel.id=rc.release_id WHERE rc.project_id=? AND rc.active=1`, p.id)
		if err != nil {
			return nil, err
		}
		for activeRows.Next() {
			var version int
			if err := activeRows.Scan(&version); err != nil {
				activeRows.Close()
				return nil, err
			}
			keep[p.slug][version] = true
		}
		activeRows.Close()
	}
	return keep, nil
}

// inUseImages returns the image IDs and tags any container currently
// references, running or not. Docker refuses to delete these anyway; knowing
// them up front turns a guaranteed error into a skip.
func (r *Reclaimer) inUseImages(ctx context.Context) (map[string]bool, error) {
	containers, err := r.Docker.Containers(ctx, true)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, item := range containers {
		if item.ImageID != "" {
			used[item.ImageID] = true
		}
		if item.Image != "" {
			used[item.Image] = true
		}
	}
	return used, nil
}

// Run performs one reclamation pass.
func (r *Reclaimer) Run(ctx context.Context) (Result, error) {
	result := Result{Items: []Item{}, DryRun: r.Policy.DryRun}
	// Bracket the pass with the daemon's own accounting so the reported figure
	// is what the disk actually gives back, not the sum of overlapping layers.
	before, beforeErr := r.Docker.DiskUsage(ctx)
	keep, err := r.protectedVersions(ctx)
	if err != nil {
		return result, fmt.Errorf("determine protected releases: %w", err)
	}
	used, err := r.inUseImages(ctx)
	if err != nil {
		return result, fmt.Errorf("inspect containers: %w", err)
	}
	images, err := r.Docker.Images(ctx)
	if err != nil {
		return result, fmt.Errorf("list images: %w", err)
	}

	for _, image := range images {
		if image.Dangling() {
			// An untagged image is a layer set nothing can name any more —
			// every rebuild of an existing tag leaves one. It is only safe to
			// drop when no container holds it.
			if used[image.ID] {
				continue
			}
			r.consider(ctx, &result, Item{Kind: "image", Ref: image.ID, Reason: "untagged layers left by a rebuild", SizeBytes: image.SizeBytes})
			continue
		}
		for _, tag := range image.Tags {
			parsed, ok := parseReleaseTag(tag)
			if !ok {
				// Not a tag Asgard produced. Upstream images a project pulls
				// are shared and not ours to reap.
				continue
			}
			if used[tag] || used[image.ID] {
				continue
			}
			switch {
			case !keep.knownSlug(parsed.slug):
				r.consider(ctx, &result, Item{Kind: "image", Ref: tag, Reason: "project " + parsed.slug + " no longer exists", SizeBytes: image.SizeBytes})
			case keep.has(parsed.slug, parsed.version):
				result.KeptForRollback++
			default:
				r.consider(ctx, &result, Item{Kind: "image", Ref: tag, Reason: fmt.Sprintf("release r%d is older than the %d kept for rollback", parsed.version, r.keepReleases()), SizeBytes: image.SizeBytes})
			}
		}
	}

	if r.Policy.BuildCacheBytes > 0 && !r.Policy.DryRun {
		reclaimed, err := r.Docker.PruneBuildCache(ctx, r.Policy.BuildCacheBytes)
		if err != nil {
			r.logger().Warn("build cache could not be pruned", "error", err)
			result.Errors++
		} else {
			result.BuildCacheBytes = reclaimed
		}
	}

	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].SizeBytes > result.Items[j].SizeBytes })
	if !r.Policy.DryRun && beforeErr == nil {
		if after, err := r.Docker.DiskUsage(ctx); err == nil {
			if freed := before.Total() - after.Total(); freed > 0 {
				result.FreedBytes = freed
			}
		}
	}
	return result, nil
}

// consider removes one artifact, or records it as a candidate under a dry run.
func (r *Reclaimer) consider(ctx context.Context, result *Result, item Item) {
	if r.Policy.DryRun {
		result.Items = append(result.Items, item)
		result.ApparentBytes += item.SizeBytes
		return
	}
	if err := r.Docker.RemoveImage(ctx, item.Ref); err != nil {
		// A concurrent deployment can start using an image between the listing
		// and the removal. Losing that race is normal and is not an error worth
		// failing the sweep over.
		item.Error = err.Error()
		result.Errors++
		result.Items = append(result.Items, item)
		r.logger().Debug("image could not be reclaimed", "ref", item.Ref, "error", err)
		return
	}
	item.Removed = true
	result.Items = append(result.Items, item)
	result.ImagesRemoved++
	result.ApparentBytes += item.SizeBytes
}

// ForgetProject removes every image a deleted project ever built.
//
// Project deletion already removed containers, networks, routes, and the source
// tree; the images it had built were the one thing left behind, and nothing
// else would ever collect them because the retention policy is keyed on a
// project that no longer exists.
func (r *Reclaimer) ForgetProject(ctx context.Context, slug string) (Result, error) {
	result := Result{Items: []Item{}}
	used, err := r.inUseImages(ctx)
	if err != nil {
		return result, err
	}
	images, err := r.Docker.Images(ctx)
	if err != nil {
		return result, err
	}
	for _, image := range images {
		for _, tag := range image.Tags {
			parsed, ok := parseReleaseTag(tag)
			if !ok || parsed.slug != slug || used[tag] || used[image.ID] {
				continue
			}
			r.consider(ctx, &result, Item{Kind: "image", Ref: tag, Reason: "project " + slug + " was deleted", SizeBytes: image.SizeBytes})
		}
	}
	return result, nil
}

// Summary renders a one-line report for an operation log.
func (r Result) Summary() string {
	if r.DryRun {
		// A dry run cannot measure; say so rather than quote the inflated sum
		// as if it were disk that will come back.
		return fmt.Sprintf("%d artifacts removable, up to %s (shared layers make the real figure lower)", len(r.Items), humanBytes(r.ApparentBytes))
	}
	parts := []string{fmt.Sprintf("reclaimed %s", humanBytes(r.FreedBytes))}
	if r.ImagesRemoved > 0 {
		parts = append(parts, fmt.Sprintf("%d images", r.ImagesRemoved))
	}
	if r.BuildCacheBytes > 0 {
		parts = append(parts, fmt.Sprintf("%s of build cache", humanBytes(r.BuildCacheBytes)))
	}
	if r.KeptForRollback > 0 {
		parts = append(parts, fmt.Sprintf("%d images kept for rollback", r.KeptForRollback))
	}
	if r.Errors > 0 {
		parts = append(parts, fmt.Sprintf("%d could not be removed", r.Errors))
	}
	return strings.Join(parts, ", ")
}

func humanBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGT"[exp])
}
