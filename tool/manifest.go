package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type OrderedMap struct {
	Keys []string
	Vals map[string]string
}

func (o *OrderedMap) UnmarshalJSON(b []byte) error {
	o.Vals = map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(b))
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

type Route struct {
	Path        string  `json:"path"`
	Upstream    string  `json:"upstream"`
	Websocket   bool    `json:"websocket"`
	Cors        bool    `json:"cors"`
	MaxBodySize *string `json:"maxBodySize"`

	fallback      bool
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
	BindMounts     []string    `json:"bindMounts"`
}

type Compatibility struct {
	Nucleus string `json:"nucleus"`
}

type Manifest struct {
	ID     string  `json:"id"`
	Route  string  `json:"route"`
	Nginx  *Nginx  `json:"nginx"`
	Server *Server `json:"server"`

	Version         string         `json:"version"`
	ManifestVersion int            `json:"manifestVersion"`
	Compatibility   *Compatibility `json:"compatibility"`

	dir  string
	role string
}

func (m *Manifest) label() string {
	if m.ID != "" {
		return m.ID
	}
	if m.Server != nil && m.Server.Service != "" {
		return m.Server.Service
	}
	return m.dir
}

func (m *Manifest) routes() []Route {
	if m.Nginx == nil {
		return nil
	}
	return m.Nginx.Routes
}

func isIgnored(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "nucleus.ignore"))
	return err == nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

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
			continue
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

func coreServices(p paths) []*Manifest {
	authMounts := []string{
		"../apps:/apps:ro",
		"../core/locales:/core-locales:ro",
		"../hub/locales:/hub-locales:ro",
		"../state:/srv/state",
	}
	if optionalModules(p).plugins {
		authMounts = append(authMounts, "../plugins:/app/plugins:ro")
	}
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
					Keys: []string{"MONGODB_URI", "JWT_SECRET", "APPS_DIR", "CORE_LOCALES_DIR", "HUB_LOCALES_DIR", "STATE_DIR"},
					Vals: map[string]string{
						"MONGODB_URI":      "mongodb://mongo:27017/nucleus",
						"JWT_SECRET":       "${JWT_SECRET:-nucleus-jwt-secret}",
						"APPS_DIR":         "/apps",
						"CORE_LOCALES_DIR": "/core-locales",
						"HUB_LOCALES_DIR":  "/hub-locales",
						"STATE_DIR":        "/srv/state",
					},
				},
				Depends:    []string{"mongo"},
				BindMounts: authMounts,
			},
		},
	}
}
