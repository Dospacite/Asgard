package networking

import (
	"strings"
	"testing"
)

func TestNormalizeAliasAndDefaultAlias(t *testing.T) {
	cases := map[string]string{
		"  Billing API_v2.example  ": "billing-api-v2-example",
		"UPPER---case":               "upper-case",
		"日本語":                        "",
		"alpha. beta":                "alpha-beta",
	}
	for input, expected := range cases {
		if actual := NormalizeAlias(input); actual != expected {
			t.Errorf("NormalizeAlias(%q) = %q, want %q", input, actual, expected)
		}
	}
	alias := DefaultAlias("Payments Project", strings.Repeat("service", 20))
	if !strings.HasPrefix(alias, "payments-project--service") {
		t.Fatalf("default alias = %q", alias)
	}
	if len(alias) > 63 {
		t.Fatalf("default alias length = %d", len(alias))
	}
}
