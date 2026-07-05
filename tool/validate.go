package main

import (
	"fmt"
	"os"
	"regexp"
)

// semverRe matches the Nucleus-supported SemVer form: MAJOR.MINOR.PATCH with an
// optional -alpha.N / -beta.N / -rc.N prerelease. Mirrors core/version.js.
var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?$`)

// provisionedServices are the backing services generate can auto-provision.
// Anything an app/widget depends on must be one of these or another declared
// server, else compose would reference a service that's never generated.
var provisionedServices = []string{"mongo", "redis", "minio"}

// validate fails the build loudly on manifest mistakes that would otherwise
// emit a broken nginx config or invalid compose file: duplicate location blocks,
// duplicate service keys, or a depends_on pointing at a never-generated service.
// On any error it prints all of them and exits 1, matching generate.js.
func validate(p paths, apps, widgets []*Manifest) {
	all := append(append(append([]*Manifest{}, apps...), widgets...), coreServices(p)...)
	var errs, warnings []string

	// Duplicate nginx paths → duplicate location {} blocks (nginx won't load).
	pathOwners := map[string][]string{}
	var pathOrder []string
	for _, m := range all {
		for _, r := range m.routes() {
			if _, ok := pathOwners[r.Path]; !ok {
				pathOrder = append(pathOrder, r.Path)
			}
			pathOwners[r.Path] = append(pathOwners[r.Path], m.label())
		}
	}
	for _, path := range pathOrder {
		if owners := pathOwners[path]; len(owners) > 1 {
			errs = append(errs, fmt.Sprintf("duplicate nginx route %q claimed by: %s", path, join(owners)))
		}
	}

	// Duplicate service names → duplicate compose service keys (invalid compose).
	svcOwners := map[string][]string{}
	var svcOrder []string
	for _, m := range all {
		if m.Server != nil && m.Server.Service != "" {
			svc := m.Server.Service
			if _, ok := svcOwners[svc]; !ok {
				svcOrder = append(svcOrder, svc)
			}
			svcOwners[svc] = append(svcOwners[svc], m.label())
		}
	}
	for _, svc := range svcOrder {
		if owners := svcOwners[svc]; len(owners) > 1 {
			errs = append(errs, fmt.Sprintf("duplicate server service %q declared by: %s", svc, join(owners)))
		}
	}

	// depends_on must resolve to a provisioned service or another declared one.
	known := map[string]bool{}
	for _, s := range provisionedServices {
		known[s] = true
	}
	for svc := range svcOwners {
		known[svc] = true
	}
	for _, m := range all {
		if m.Server == nil {
			continue
		}
		for _, dep := range m.Server.Depends {
			if !known[dep] {
				errs = append(errs, fmt.Sprintf("%s depends on unknown service %q — not another app's server and not auto-provisioned (%s)",
					m.label(), dep, joinSlash(provisionedServices)))
			}
		}
	}

	// An app with a route but no matching location won't be served as an SPA.
	for _, m := range append(append([]*Manifest{}, apps...), widgets...) {
		if m.Route == "" {
			continue
		}
		found := false
		for _, r := range m.routes() {
			if r.Path == m.Route {
				found = true
				break
			}
		}
		if !found {
			warnings = append(warnings, fmt.Sprintf("%s sets route %q but no nginx route has that path — it won't be served as an SPA in prod", m.label(), m.Route))
		}
	}

	// Versioning hygiene — warn (never fail) so the ecosystem can adopt SemVer
	// gradually. Every installable module should declare a valid version, a
	// manifestVersion, and a nucleus compatibility range. See infra/nucleus-docs/VERSIONING.md.
	for _, m := range append(append([]*Manifest{}, apps...), widgets...) {
		if m.Version == "" {
			warnings = append(warnings, fmt.Sprintf("%s has no \"version\" — every module should declare a SemVer version", m.label()))
		} else if !semverRe.MatchString(m.Version) {
			warnings = append(warnings, fmt.Sprintf("%s version %q is not valid SemVer (expected e.g. 0.1.0 or 1.0.0-beta.2)", m.label(), m.Version))
		}
		if m.ManifestVersion == 0 {
			warnings = append(warnings, fmt.Sprintf("%s has no \"manifestVersion\" — set it to 1", m.label()))
		}
		if m.Compatibility == nil || m.Compatibility.Nucleus == "" {
			warnings = append(warnings, fmt.Sprintf("%s has no \"compatibility.nucleus\" range — declare which platform versions it supports", m.label()))
		}
	}

	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "\n✖ Manifest validation failed — fix these and rebuild:")
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %s\n", e)
		}
		os.Exit(1)
	}
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

func joinSlash(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += "/"
		}
		out += v
	}
	return out
}
