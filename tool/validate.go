package main

import (
	"fmt"
	"os"
)

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
