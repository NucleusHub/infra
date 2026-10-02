package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const createDownload = `{
  "$schema": "https://nucleus.example/schema/build-1.json",
  "generated": "2026-09-27T10:00:00.000Z",
  "name": "Home lab",
  "appearance": {"theme": "light", "accent": "ember", "customAccent": "#A855F7", "radius": 40, "motion": "measured",
    "glass": "solid", "transparency": 0.4, "typeScale": 1.05, "wallpaper": "grid"},
  "smart": true,
  "packages": [
    {"id": "core", "kind": "core", "version": "1.0.0"},
    {"id": "hub", "kind": "app", "version": "0.2.4"},
    {"id": "echo", "kind": "app", "version": "0.5.3", "options": {"retention": "30d"}},
    {"id": "echo-widget", "kind": "widget", "version": "0.1.0"},
    {"id": "sys-load", "kind": "widget", "version": "0.1.0"},
    {"id": "binders", "kind": "plugin", "version": "0.1.0"},
    {"id": "dex", "kind": "app", "version": "0.1.0"},
    {"id": "photos", "kind": "app", "version": "1.0.0"}
  ]
}`

func TestResolveBuild(t *testing.T) {
	mods, _, _ := buildCatalog(feedFixture())
	mods["app:dex"].Installed = true
	b, err := parseBuild([]byte(createDownload))
	if err != nil {
		t.Fatal(err)
	}
	imp := resolveBuild(mods, b)

	if want := []string{"app:echo", "widget:echo", "widget:sys-load", "plugin:binders"}; !reflect.DeepEqual(imp.Install, want) {
		t.Errorf("install = %v, want %v (core and hub skipped, slugs mapped to module keys)", imp.Install, want)
	}
	if !reflect.DeepEqual(imp.Present, []string{"app:dex"}) || !reflect.DeepEqual(imp.Unknown, []string{"photos"}) {
		t.Errorf("present = %v, unknown = %v", imp.Present, imp.Unknown)
	}
	notes := strings.Join(imp.Notes, "\n")
	for _, want := range []string{"photos", "settings", "Echo", "Smart"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes should mention %q: %v", want, imp.Notes)
		}
	}
	a := imp.Appearance
	if a == nil || a.Name != "Home lab" || a.Theme != "light" || a.Accent != "ember" || a.AccentColor != "#F2711C" ||
		a.Radius != 26 || a.Glass != "solid" || a.Wallpaper != "grid" || a.TypeScale != 1.05 {
		t.Errorf("appearance = %+v", a)
	}

	pl := planModules(mods, imp.Install, nil, planOpts{})
	if len(pl.Errors) > 0 {
		t.Fatalf("an imported build should plan cleanly: %v", pl.Errors)
	}
	got := keys(pl.Install)
	for _, want := range []string{"widget:sysinfo", "widget:core", "service:plugin-runtime"} {
		if !contains(got, want) {
			t.Errorf("plan should pull in dependency %s: %v", want, got)
		}
	}
}

func TestParseSavedConfiguration(t *testing.T) {
	b, err := parseBuild([]byte(`{"version":1,"name":"Saved","installed":["core","hub","echo"],"options":{},"appearance":{"accent":"custom","customAccent":"#0af"}}`))
	if err != nil {
		t.Fatal(err)
	}
	mods, _, _ := buildCatalog(feedFixture())
	imp := resolveBuild(mods, b)
	if !reflect.DeepEqual(imp.Install, []string{"app:echo"}) {
		t.Errorf("install = %v", imp.Install)
	}
	if a := imp.Appearance; a.Accent != "custom" || a.AccentColor != "#00AAFF" || a.AccentSoft != "#59C8FF" {
		t.Errorf("custom accent = %+v", a)
	}

	for _, bad := range []string{`nope`, `{}`, `{"name":"empty"}`} {
		if _, err := parseBuild([]byte(bad)); err == nil {
			t.Errorf("parseBuild(%s) should fail", bad)
		}
	}
}

func TestNormalizeAppearance(t *testing.T) {
	a, notes := normalizeAppearance(map[string]any{"theme": "sepia", "accent": "violet", "frosted": false, "typeScale": 3.0})
	if a.Theme != "dark" || a.Accent != "nucleus" || a.Glass != "clear" || a.TypeScale != 1.15 {
		t.Errorf("fallbacks = %+v", a)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "sepia") {
		t.Errorf("only the unknown theme should be reported (violet is a retired accent): %v", notes)
	}
	if _, notes := normalizeAppearance(map[string]any{"accent": "custom", "customAccent": "red"}); len(notes) != 1 {
		t.Errorf("a custom accent that isn't a colour should be reported: %v", notes)
	}
}

func TestAppearanceFile(t *testing.T) {
	p := paths{root: t.TempDir()}
	if a, err := readAppearance(p); a != nil || err != nil {
		t.Fatalf("no file should read as stock: %v %v", a, err)
	}
	want := defaultAppearance()
	want.Name = "Mine"
	if err := writeAppearance(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := readAppearance(p)
	if err != nil || got.Stamp == "" {
		t.Fatalf("a written appearance needs a stamp: %+v %v", got, err)
	}
	want.Stamp = got.Stamp
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}

func TestImportHandler(t *testing.T) {
	p := testTree(t)
	state := &moduleState{p: p, org: "acme", at: time.Now(),
		cached: map[string]*module{"widget:clock": {Kind: kindWidget, ID: "clock", Slug: "clock", Name: "Clock", Repo: "widgets", Requires: []string{}, Bundles: []string{}}}}
	srv := httptest.NewServer(modulesHandler(p, state, "s3cret"))
	defer srv.Close()
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Modules-Token", "s3cret")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	if code, out := post("/api/import", `{"name":"x"}`); code != 400 || out["error"] == nil {
		t.Errorf("a file that isn't a build should be refused with a reason: %d %v", code, out)
	}
	code, out := post("/api/import", `{"name":"Desk","packages":[{"id":"core"},{"id":"clock","kind":"widget"}],"appearance":{"accent":"forest"}}`)
	if code != 200 || !reflect.DeepEqual(out["install"], []any{"widget:clock"}) || out["appearance"].(map[string]any)["accentColor"] != "#2FA36B" {
		t.Fatalf("import: %d %v", code, out)
	}

	look, _ := json.Marshal(out["appearance"])
	tampered := strings.Replace(string(look), `"radius":16`, `"radius":999`, 1)
	if code, _ := post("/api/run", `{"install":[],"remove":[],"mode":"none","appearance":`+tampered+`}`); code != 200 {
		t.Fatalf("run: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest("GET", srv.URL+"/api/job", nil)
		req.Header.Set("X-Modules-Token", "s3cret")
		res, _ := http.DefaultClient.Do(req)
		var j map[string]any
		json.NewDecoder(res.Body).Decode(&j)
		res.Body.Close()
		if j["running"] == false {
			if j["ok"] != true {
				t.Fatalf("run failed: %v", j["log"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a, err := readAppearance(p)
	if err != nil || a == nil || a.Name != "Desk" || a.Accent != "forest" || a.Radius != 26 {
		t.Errorf("run should write the appearance, clamped again: %+v %v", a, err)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
