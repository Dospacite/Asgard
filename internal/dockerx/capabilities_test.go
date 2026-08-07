package dockerx

import (
	"slices"
	"testing"
)

func TestWorkloadCapabilitiesStayMinimal(t *testing.T) {
	want := []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"}
	if got := workloadCapabilities(); !slices.Equal(got, want) {
		t.Fatalf("unexpected workload capabilities: %v", got)
	}
}
