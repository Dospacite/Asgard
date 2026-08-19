package store

// HSTS policy for a public route.
//
// Asgard used to stamp one shared middleware — max-age=31536000,
// includeSubDomains, preload — onto every router. Inside a wildcard domain the
// control plane owns, that is fine: every name under it is Asgard's to commit.
// Once a service can claim any DNS name it stops being fine. Serving that
// header on someone's apex silently commits every subdomain of their domain to
// HTTPS-only, including hosts Asgard has never heard of and does not serve,
// and `preload` asks browser vendors to bake it in — removal from the preload
// list takes months and is not something the operator can undo from here.
//
// The strong form is a deliberate choice now. The mode lives on the service
// rather than the route because the deployer drops and recreates a project's
// routes on every release.
const (
	// HSTSAuto derives the policy from the hostname's zone: names inside the
	// control plane's own wildcard domain get the strong form, everything else
	// gets a plain max-age. It is the default for every service.
	HSTSAuto = ""
	// HSTSStandard sends max-age for this host only.
	HSTSStandard = "standard"
	// HSTSStrict adds includeSubDomains and preload. Choosing it for a custom
	// domain commits every subdomain of that domain, including ones Asgard does
	// not serve, for as long as the max-age — and, once preloaded, longer.
	HSTSStrict = "strict"
	// HSTSOff sends no Strict-Transport-Security header, for a name something
	// else also serves over plain HTTP.
	HSTSOff = "off"
)

// ValidHSTSMode reports whether a stored or submitted mode is one Asgard knows.
func ValidHSTSMode(mode string) bool {
	switch mode {
	case HSTSAuto, HSTSStandard, HSTSStrict, HSTSOff:
		return true
	}
	return false
}
