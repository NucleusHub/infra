package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTree lays out a minimal Nucleus checkout (core + infra + hub) in a temp
// dir; tests add optional modules on top.
func testTree(t *testing.T) paths {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"core", "infra", "hub"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	return paths{infra: filepath.Join(root, "infra"), root: root, apps: filepath.Join(root, "apps"), widgets: filepath.Join(root, "widgets")}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMinimalSetupHasNoOptionalModules(t *testing.T) {
	p := testTree(t)
	// Empty dirs (what Docker leaves behind for a missing bind-mount source) must
	// not count as installed modules.
	os.MkdirAll(filepath.Join(p.root, "plugins"), 0o755)
	os.MkdirAll(p.widgets, 0o755)

	if m := optionalModules(p); m.plugins || m.widgets || m.pluginRuntime {
		t.Fatalf("minimal setup reported modules: %+v", m)
	}
	override := generateOverride(p, nil, nil, nil)
	for _, bad := range []string{"plugin-runtime", "../plugins", "../widgets"} {
		if strings.Contains(override, bad) {
			t.Errorf("minimal override mentions %q:\n%s", bad, override)
		}
	}
	if !strings.Contains(pluginsLocation(p), `return 200 '{"plugins":[]}'`) {
		t.Error("without plugin-runtime, /api/plugins should answer an empty registry")
	}
}

func TestOptionalModulesAreWiredWhenInstalled(t *testing.T) {
	p := testTree(t)
	write(t, filepath.Join(p.root, "plugins", "foo", "nucleus.plugin.json"), `{"id":"foo"}`)
	write(t, filepath.Join(p.widgets, "package.json"), `{}`)
	write(t, filepath.Join(p.root, "plugin-runtime", "Dockerfile"), "FROM node")

	m := optionalModules(p)
	if !m.plugins || !m.widgets || !m.pluginRuntime {
		t.Fatalf("installed modules not detected: %+v", m)
	}
	override := generateOverride(p, nil, nil, []hubLib{{id: "dash", rel: "../apps/dash/client"}})
	for _, want := range []string{
		"../apps/dash/client:/app/libs/dash:ro",
		"../widgets:/app/widgets:ro",
		"../plugins:/app/plugins:ro",
		`NUCLEUS_PLUGINS: "foo"`,
		"  plugin-runtime:\n",
		"      - plugin-runtime\n",
	} {
		if !strings.Contains(override, want) {
			t.Errorf("override missing %q:\n%s", want, override)
		}
	}
	if !strings.Contains(pluginsLocation(p), "plugin-runtime:4100") {
		t.Error("with plugin-runtime installed, /api/plugins should proxy to it")
	}
}

func TestSyncHubLibLinksAddsAndPrunes(t *testing.T) {
	p := testTree(t)
	os.MkdirAll(filepath.Join(p.apps, "dash", "client"), 0o755)
	libs := filepath.Join(p.root, "hub", "libs")

	syncHubLibLinks(p, []hubLib{{id: "dash"}})
	resolved, err := filepath.EvalSymlinks(filepath.Join(libs, "dash"))
	want, _ := filepath.EvalSymlinks(filepath.Join(p.apps, "dash", "client"))
	if err != nil || resolved != want {
		t.Fatalf("hub/libs/dash → %q, %v; want %q", resolved, err, want)
	}

	// A real file in hub/libs is never touched; a stale link is pruned.
	write(t, filepath.Join(libs, "keep.txt"), "x")
	syncHubLibLinks(p, nil)
	if _, err := os.Lstat(filepath.Join(libs, "dash")); !os.IsNotExist(err) {
		t.Error("stale hub lib link was not pruned")
	}
	if !exists(filepath.Join(libs, "keep.txt")) {
		t.Error("prune removed a real file")
	}
}
