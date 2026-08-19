package importer

import (
	"strings"
	"testing"
)

func TestFirstCommitPrefersTheBranch(t *testing.T) {
	// ls-remote for a name that exists as both a branch and a tag returns both.
	// A deployment tracks the branch, so the branch is what must be compared.
	output := strings.Join([]string{
		"1111111111111111111111111111111111111111\trefs/tags/main",
		"2222222222222222222222222222222222222222\trefs/heads/main",
	}, "\n")
	if got := firstCommit(output, "main"); got != "2222222222222222222222222222222222222222" {
		t.Fatalf("got %s, want the branch head", got)
	}
}

func TestFirstCommitReadsHEADForTheDefaultBranch(t *testing.T) {
	output := "ref: refs/heads/trunk\tHEAD\n3333333333333333333333333333333333333333\tHEAD\n"
	if got := firstCommit(output, ""); got != "3333333333333333333333333333333333333333" {
		t.Fatalf("got %q, want the HEAD commit", got)
	}
}

func TestFirstCommitIgnoresJunk(t *testing.T) {
	if got := firstCommit("not a ref line\n\n", "main"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// The summary is what lands in the operation log, and it is the only place the
// "push, then deploy ships the old commit" trap becomes visible.
func TestSummaryNamesTheCommitBeingBuilt(t *testing.T) {
	behind := SourceStatus{Commit: "aaaaaaaaaaaabbbb", RemoteCommit: "ccccccccccccdddd", Ref: "main", Behind: true, Checked: true}
	message := behind.Summary()
	if !strings.Contains(message, "aaaaaaaaaaaa") || !strings.Contains(message, "cccccccccccc") {
		t.Fatalf("summary must name both commits: %s", message)
	}
	if !strings.Contains(message, "Re-sync") {
		t.Fatalf("summary must say what to do about it: %s", message)
	}

	current := SourceStatus{Commit: "aaaaaaaaaaaabbbb", RemoteCommit: "aaaaaaaaaaaabbbb", Ref: "main", Checked: true}
	if message := current.Summary(); strings.Contains(message, "Re-sync") {
		t.Fatalf("an up-to-date tree must not warn: %s", message)
	}

	// An unreachable remote is not a failure; it just means the comparison is
	// unavailable, and the commit being built is still worth reporting.
	unchecked := SourceStatus{Commit: "aaaaaaaaaaaabbbb", Reason: "the remote could not be read"}
	if message := unchecked.Summary(); !strings.Contains(message, "aaaaaaaaaaaa") || !strings.Contains(message, "not consulted") {
		t.Fatalf("unexpected summary: %s", message)
	}
}

func TestSummaryHandlesAnUnrecordedRevision(t *testing.T) {
	if message := (SourceStatus{}).Summary(); !strings.Contains(message, "no source revision") {
		t.Fatalf("unexpected summary: %s", message)
	}
}
