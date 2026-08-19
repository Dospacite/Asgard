package dockerx

import "testing"

func TestThrottledPercent(t *testing.T) {
	cases := []struct {
		name               string
		periods, throttled int64
		want               float64
	}{
		// The case this whole change exists for: a service averaging near-zero
		// CPU while stopped at its quota in 42% of scheduling periods.
		{"bursty workload against its quota", 30646, 12880, 42.028976}, //nolint:mnd // the observed figures
		{"never throttled", 30646, 0, 0},
		{"always throttled", 100, 100, 100},
		// No quota means no scheduling periods, which is not the same as never
		// being throttled — but it is the only honest answer available.
		{"no quota configured", 0, 0, 0},
		// Counters read across a container restart can look impossible.
		{"more throttled than total is clamped", 100, 250, 100},
		{"negative counters are ignored", -5, -5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ThrottledPercent(tc.periods, tc.throttled)
			if diff := got - tc.want; diff > 0.001 || diff < -0.001 {
				t.Fatalf("ThrottledPercent(%d, %d) = %f, want %f", tc.periods, tc.throttled, got, tc.want)
			}
		})
	}
}
