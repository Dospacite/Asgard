package reclaim

import (
	"testing"

	"github.com/rousoftware/asgard/internal/dockerx"
)

func TestParseReleaseTag(t *testing.T) {
	cases := []struct {
		tag     string
		ok      bool
		slug    string
		service string
		version int
	}{
		{"asgard/rouwriteups/web:r8", true, "rouwriteups", "web", 8},
		{"asgard/ikincikat-internal/minio-init:r7", true, "ikincikat-internal", "minio-init", 7},
		{"asgard/asq-syncer/worker:r12", true, "asq-syncer", "worker", 12},
		// Anything Asgard did not build is not Asgard's to delete. These are
		// the images projects pull and share; reaping them would break
		// unrelated containers and force a re-pull.
		{"postgres:16-alpine", false, "", "", 0},
		{"traefik:v3.6", false, "", "", 0},
		{"ghcr.io/acme/app:1.4.2", false, "", "", 0},
		{"asgard-control-plane:local", false, "", "", 0},
		// A near-miss must not be mistaken for a release tag.
		{"asgard/rouwriteups/web:latest", false, "", "", 0},
		{"asgard/rouwriteups/web", false, "", "", 0},
		{"notasgard/rouwriteups/web:r8", false, "", "", 0},
		{"asgard/rouwriteups/web:r", false, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			parsed, ok := parseReleaseTag(tc.tag)
			if ok != tc.ok {
				t.Fatalf("parseReleaseTag(%q) ok = %v, want %v", tc.tag, ok, tc.ok)
			}
			if !ok {
				return
			}
			if parsed.slug != tc.slug || parsed.service != tc.service || parsed.version != tc.version {
				t.Fatalf("got %+v, want %s/%s r%d", parsed, tc.slug, tc.service, tc.version)
			}
		})
	}
}

func TestKeepSet(t *testing.T) {
	keep := keepSet{"blog": {8: true, 7: true}, "fresh": {}}
	if !keep.has("blog", 8) || !keep.has("blog", 7) {
		t.Fatal("retained versions should be protected")
	}
	if keep.has("blog", 6) {
		t.Fatal("an aged-out version should not be protected")
	}
	// A project with no releases yet must still be a known slug, or its future
	// images would be treated as orphans of a deleted project.
	if !keep.knownSlug("fresh") {
		t.Fatal("a project without releases is still a live project")
	}
	if keep.knownSlug("deleted") {
		t.Fatal("a slug with no project row is an orphan")
	}
}

func TestDanglingDetection(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want bool
	}{
		{"no tags at all", nil, true},
		{"empty tag list", []string{}, true},
		{"explicit none", []string{"<none>:<none>"}, true},
		{"a real tag", []string{"asgard/blog/web:r8"}, false},
		{"one real tag among placeholders", []string{"<none>:<none>", "asgard/blog/web:r8"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (dockerx.Image{Tags: tc.tags}).Dangling(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKeepReleasesFloor(t *testing.T) {
	// Zero or negative retention would delete the image of the release that is
	// currently running. The floor exists so a misconfiguration cannot take
	// production down.
	for _, value := range []int{-5, 0, 1} {
		r := &Reclaimer{Policy: Policy{KeepReleases: value}}
		if got := r.keepReleases(); got < 1 {
			t.Fatalf("KeepReleases=%d produced %d, which would reap a live image", value, got)
		}
	}
	r := &Reclaimer{Policy: Policy{KeepReleases: 5}}
	if got := r.keepReleases(); got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1 << 20: "1.0 MiB", 9<<30 + 1<<29: "9.5 GiB"}
	for value, want := range cases {
		if got := humanBytes(value); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestSummaryReportsRollbackProtection(t *testing.T) {
	result := Result{ImagesRemoved: 4, ApparentBytes: 9 << 30, FreedBytes: 3 << 30, BuildCacheBytes: 1 << 30, KeptForRollback: 6}
	got := result.Summary()
	for _, want := range []string{"3.0 GiB", "4 images", "6 images kept for rollback"} {
		if !contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
	// The measured figure must be the one reported, never the inflated sum of
	// per-image sizes — release images of one service share nearly every layer.
	if contains(got, "9.0 GiB") {
		t.Fatalf("summary quoted the apparent size instead of what was freed: %q", got)
	}
	dry := Result{DryRun: true, Items: make([]Item, 3), ApparentBytes: 1 << 30}
	if !contains(dry.Summary(), "removable") || !contains(dry.Summary(), "up to") {
		t.Fatalf("a dry run must not claim it reclaimed anything: %q", dry.Summary())
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
