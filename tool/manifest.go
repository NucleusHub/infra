package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// OrderedMap is a string→string map that remembers insertion (JSON) order.
// The env blocks generate.js emitted iterated keys in manifest order, so we must
// preserve it to stay byte-identical.
type OrderedMap struct {
	Keys []string
	Vals map[string]string
}

func (o *OrderedMap) UnmarshalJSON(b []byte) error {
	o.Vals = map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(b))
	// opening '{'
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key := keyTok.(string)
		var val string
		if err := dec.Decode(&val); err != nil {
			return err
		}
		o.Keys = append(o.Keys, key)
		o.Vals[key] = val
	}
	return nil
}

// Route mirrors one entry of manifest.nginx.routes plus the two fields
// generate.js attached dynamically during generation.
type Route struct {
	Path        string  `json:"path"`
	Upstream    string  `json:"upstream"`
	Websocket   bool    `json:"websocket"`
	Cors        bool    `json:"cors"`
	MaxBodySize *string `json:"maxBodySize"`

	// fallback is computed (not from JSON): the SPA/client route falls back to
	// the Anchor degraded page when its upstream is down.
	fallback bool
	// manifestRoute is the owning manifest's `route`, used to tell SPA routes
	// (path === route) from API/proxy routes in prod generation.
	manifestRoute string
}

type Nginx struct {
	Routes []Route `json:"routes"`
}

type Server struct {
	Service        string      `json:"service"`
	Context        string      `json:"context"`
	Port           int         `json:"port"`
	Env            *OrderedMap `json:"env"`
	Depends        []string    `json:"depends"`
	NamedVolumes   []string    `json:"namedVolumes"`
	HealthEndpoint string      `json:"healthEndpoint"`
	StartPeriod    string      `json:"startPeriod"`
	// Raw bind-mount lines (e.g. "../apps:/apps:ro") emitted verbatim into the
	// prod service's volumes. Unlike NamedVolumes these are NOT declared as
	// top-level named volumes. Internal-only (set by coreServices, not JSON).
	BindMounts []string `json:"-"`
}

type Manifest struct {
	ID     string  `json:"id"`
	Route  string  `json:"route"`
	Nginx  *Nginx  `json:"nginx"`
	Server *Server `json:"server"`

	dir  string // _dir — absolute path to the app/widget directory
	role string // _role — overrides the compose label role (auth, widget-server)
}

// label is the human-readable identifier used in validation messages.
func (m *Manifest) label() string {
	if m.ID != "" {
		return m.ID
	}
	if m.Server != nil && m.Server.Service != "" {
		return m.Server.Service
	}
	return m.dir
}

// routes returns the manifest's nginx routes (nil-safe).
func (m *Manifest) routes() []Route {
	if m.Nginx == nil {
		return nil
	}
	return m.Nginx.Routes
}

// isIgnored reports whether dir carries a nucleus.ignore marker, which excludes
// it from ALL discovery (registry, nginx/compose, hub symlinking). Keeps
// apps/anchor out of the ecosystem. Mirrors the guard in build + registry.
func isIgnored(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "nucleus.ignore"))
	return err == nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readManifests discovers app/widget manifests under baseDir. Directories are
// scanned in sorted order so output is deterministic (matches the alphabetical
// readdir the JS tool relied on).
func readManifests(baseDir, filename string) ([]*Manifest, error) {
	if !exists(baseDir) {
		return nil, nil
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && !isIgnored(filepath.Join(baseDir, e.Name())) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []*Manifest
	for _, name := range names {
		dir := filepath.Join(baseDir, name)
		path := filepath.Join(dir, filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // no manifest in this dir
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			fmt.Printf("  Warning: failed to parse %s\n", path)
			continue
		}
		m.dir = dir
		out = append(out, &m)
	}
	return out, nil
}

// coreServices are always-present infrastructure services (not discoverable
// apps). They live in core/ and are hardcoded here — nginx route + prod
// container — exactly as in generate.js. The registry never sees them.
func coreServices(p paths) []*Manifest {
	return []*Manifest{
		{
			dir:  filepath.Join(p.root, "core", "auth-server"),
			role: "auth",
			Nginx: &Nginx{Routes: []Route{
				{Path: "/api/auth", Upstream: "auth-server:3005"},
			}},
			Server: &Server{
				Service:        "auth-server",
				Context:        "../core/auth-server",
				Port:           3005,
				HealthEndpoint: "/api/auth/health",
				Env: &OrderedMap{
					Keys: []string{"MONGODB_URI", "JWT_SECRET", "APPS_DIR", "CORE_LOCALES_DIR", "HUB_LOCALES_DIR"},
					Vals: map[string]string{
						"MONGODB_URI":      "mongodb://mongo:27017/nucleus",
						"JWT_SECRET":       "${JWT_SECRET:-nucleus-jwt-secret}",
						"APPS_DIR":         "/apps",
						"CORE_LOCALES_DIR": "/core-locales",
						"HUB_LOCALES_DIR":  "/hub-locales",
					},
				},
				Depends: []string{"mongo"},
				// The localization service reads shipped locale files off disk.
				BindMounts: []string{
					"../apps:/apps:ro",
					"../core/locales:/core-locales:ro",
					"../hub/locales:/hub-locales:ro",
				},
			},
		},
	}
}
