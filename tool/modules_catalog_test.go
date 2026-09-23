package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClassifyRepo(t *testing.T) {
	l, ok := classifyRepo([]string{"nucleus.app.json", "client/main.js"})
	if !ok || !l.app {
		t.Fatalf("app repo not detected: %+v", l)
	}
	l, ok = classifyRepo([]string{"README.md", "a/nucleus.plugin.json", "b/nucleus.plugin.json", "b/server/route.js", "x/y/nucleus.plugin.json"})
	if !ok || l.app || !reflect.DeepEqual(l.collections[kindPlugin], []string{"a", "b"}) {
		t.Fatalf("plugin collection wrong (nested manifests must not count): %+v", l)
	}
	if _, ok := classifyRepo([]string{"nucleus.ignore", "nucleus.app.json"}); ok {
		t.Fatal("nucleus.ignore must exclude the repo")
	}
}

func TestRemoteOrg(t *testing.T) {
	for url, want := range map[string][2]any{
		"https://github.com/NucleusHub/infra.git": {"NucleusHub", false},
		"git@github.com:NucleusHub/plugins.git":   {"NucleusHub", true},
		"":                                        {"", false},
	} {
		org, ssh := remoteOrg(url)
		if org != want[0] || ssh != want[1] {
			t.Errorf("remoteOrg(%q) = %q, %v", url, org, ssh)
		}
	}
}

// catalogFixture: an app with a bundled widget, a plugin needing an app, a
// widget depending on another, and a locked collection base.
func catalogFixture() map[string]*module {
	mk := func(kind moduleKind, id string, requires ...string) *module {
		return &module{Kind: kind, ID: id, Name: id, Repo: map[moduleKind]string{kindApp: id, kindPlugin: "plugins", kindWidget: "widgets", kindService: id}[kind],
			Requires: requires, Bundles: []string{}}
	}
	mods := map[string]*module{}
	for _, m := range []*module{
		mk(kindApp, "echo"),
		mk(kindApp, "dex"),
		mk(kindWidget, "echo"),
		mk(kindWidget, "core"),
		mk(kindWidget, "sysinfo"),
		mk(kindWidget, "sys-load", "widget:sysinfo"),
		mk(kindPlugin, "binders", "app:dex", "service:plugin-runtime"),
		mk(kindService, "plugin-runtime"),
	} {
		mods[m.key()] = m
	}
	mods["widget:core"].required, mods["widget:core"].Hidden = true, true
	linkBundles(mods)
	return mods
}

func TestPlanResolvesDependenciesAndBundles(t *testing.T) {
	mods := catalogFixture()
	if !mods["widget:echo"].Hidden || !reflect.DeepEqual(mods["app:echo"].Bundles, []string{"widget:echo"}) {
		t.Fatal("app should bundle (and hide) the widget sharing its id")
	}

	pl := planModules(mods, []string{"sys-load", "binders", "echo"}, nil, planOpts{})
	if len(pl.Errors) > 0 {
		t.Fatal(pl.Errors)
	}
	got := strings.Join(keys(pl.Install), " ")
	for _, want := range []string{"widget:core", "widget:sysinfo", "widget:sys-load", "app:dex", "service:plugin-runtime", "plugin:binders", "app:echo", "widget:echo"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan misses %s: %s", want, got)
		}
	}
	if strings.Index(got, "widget:sysinfo") > strings.Index(got, "widget:sys-load") {
		t.Errorf("dependencies must come first: %s", got)
	}
}

func TestPlanGuardsRemovals(t *testing.T) {
	mods := catalogFixture()
	for _, k := range []string{"widget:core", "widget:sysinfo", "widget:sys-load", "app:echo", "widget:echo"} {
		mods[k].Installed = true
	}
	local := &module{Kind: kindWidget, ID: "mine", Installed: true, Local: true, Requires: []string{}, Bundles: []string{}}
	mods[local.key()] = local

	if pl := planModules(mods, nil, []string{"sysinfo"}, planOpts{}); len(pl.Errors) > 0 ||
		!reflect.DeepEqual(keys(pl.Remove), []string{"widget:sysinfo", "widget:sys-load"}) || len(pl.Warnings) != 1 {
		t.Errorf("removing a dependency takes its dependents along, with a warning: %v %v %v", keys(pl.Remove), pl.Warnings, pl.Errors)
	}
	mine := &module{Kind: kindWidget, ID: "gauge", Installed: true, Local: true, Requires: []string{"widget:sysinfo"}, Bundles: []string{}}
	mods[mine.key()] = mine
	if pl := planModules(mods, nil, []string{"sysinfo"}, planOpts{}); len(pl.Errors) == 0 {
		t.Error("a cascade that would reach a local module must fail instead")
	}
	delete(mods, mine.key())
	if pl := planModules(mods, nil, []string{"sysinfo", "sys-load"}, planOpts{}); len(pl.Errors) > 0 {
		t.Errorf("removing both together is fine: %v", pl.Errors)
	}
	if pl := planModules(mods, nil, []string{"mine"}, planOpts{}); len(pl.Errors) == 0 {
		t.Error("a local module (in no repo) must never be removed")
	}
	if pl := planModules(mods, nil, []string{"widget:core"}, planOpts{}); len(pl.Errors) == 0 {
		t.Error("a collection's required entry can't be removed on its own")
	}
	pl := planModules(mods, nil, []string{"app:echo"}, planOpts{})
	if len(pl.Errors) > 0 || !reflect.DeepEqual(keys(pl.Remove), []string{"app:echo", "widget:echo"}) {
		t.Errorf("removing an app removes its bundled widget: %v %v", keys(pl.Remove), pl.Errors)
	}
	mods["service:plugin-runtime"].Installed = true
	mods["plugin:binders"].Installed = true
	mods["app:dex"].Installed = true
	if pl := planModules(mods, nil, []string{"echo"}, planOpts{}); strings.Contains(strings.Join(keys(pl.Remove), " "), "service:") {
		t.Errorf("the plugin runtime stays while a plugin needs it: %v", keys(pl.Remove))
	}
	if pl := planModules(mods, nil, []string{"binders"}, planOpts{}); !strings.Contains(strings.Join(keys(pl.Remove), " "), "service:plugin-runtime") {
		t.Errorf("removing the last plugin should remove the runtime: %v", keys(pl.Remove))
	}
	if _, err := resolveKey(mods, "echo"); err != nil {
		t.Errorf("'echo' should resolve to the app (the widget is hidden): %v", err)
	}
}

// ── End to end against local git repos ──────────────────────────────────────

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// originRepo creates a local repo with the given files, committed on main.
func originRepo(t *testing.T, base, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(base, name)
	for f, c := range files {
		write(t, filepath.Join(dir, f), c)
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "init")
	gitT(t, dir, "config", "uploadpack.allowFilter", "true")
}

func TestInstallAndRemoveEndToEnd(t *testing.T) {
	origins := t.TempDir()
	originRepo(t, origins, "notes", map[string]string{"nucleus.app.json": `{"id":"notes","version":"1.0.0"}`})
	originRepo(t, origins, "widgets", map[string]string{
		"package.json":              "{}",
		"core/nucleus.widget.json":  `{"id":"core","locked":true}`,
		"clock/nucleus.widget.json": `{"id":"clock"}`,
		"moon/nucleus.widget.json":  `{"id":"moon"}`,
	})

	p := testTree(t)
	g := gitRunner{base: "file://" + origins, out: io.Discard}
	catalog := map[string]*module{
		"app:notes":    {Kind: kindApp, ID: "notes", Repo: "notes", Requires: []string{}, Bundles: []string{}},
		"widget:core":  {Kind: kindWidget, ID: "core", Repo: "widgets", required: true, Hidden: true, Requires: []string{}, Bundles: []string{}},
		"widget:clock": {Kind: kindWidget, ID: "clock", Repo: "widgets", Requires: []string{}, Bundles: []string{}},
		"widget:moon":  {Kind: kindWidget, ID: "moon", Repo: "widgets", Requires: []string{}, Bundles: []string{}},
	}
	linkBundles(catalog)

	step := func(install, remove []string) {
		t.Helper()
		mods := mergeModules(catalog, installedModules(p))
		pl := planModules(mods, install, remove, planOpts{})
		checkRemovals(p, &pl)
		if err := executePlan(p, g, mods, pl); err != nil {
			t.Fatal(err)
		}
	}
	has := func(rel string) bool { return exists(filepath.Join(p.root, rel)) }

	step([]string{"notes", "clock"}, nil)
	if !has("apps/notes/nucleus.app.json") || !has("widgets/clock/nucleus.widget.json") || !has("widgets/core/nucleus.widget.json") {
		t.Fatal("install didn't land app + widget + required core")
	}
	if has("widgets/moon") {
		t.Fatal("a sparse install must not bring other widgets")
	}
	if m := optionalModules(p); !m.widgets {
		t.Error("infra should see the widgets module as installed")
	}

	step([]string{"moon"}, []string{"clock"})
	if has("widgets/clock") || !has("widgets/moon/nucleus.widget.json") {
		t.Fatal("swap clock → moon failed")
	}

	// Docker recreating a removed app's bind-mount source doesn't block a reinstall.
	step(nil, []string{"notes"})
	os.MkdirAll(filepath.Join(p.apps, "notes", "client"), 0o755)
	step([]string{"notes"}, nil)
	if !has("apps/notes/nucleus.app.json") {
		t.Fatal("reinstall over an empty skeleton failed")
	}

	// Uncommitted work blocks a removal.
	write(t, filepath.Join(p.apps, "notes", "draft.txt"), "wip")
	mods := mergeModules(catalog, installedModules(p))
	pl := planModules(mods, nil, []string{"notes"}, planOpts{})
	checkRemovals(p, &pl)
	if len(pl.Errors) == 0 {
		t.Fatal("removing an app with uncommitted work must be refused")
	}
	os.Remove(filepath.Join(p.apps, "notes", "draft.txt"))
	write(t, filepath.Join(p.apps, "notes", ".DS_Store"), "finder") // litter, not work

	step(nil, []string{"notes", "moon"})
	if has("apps/notes") || has("widgets") {
		t.Fatal("removing the last widget should remove the whole widgets checkout, and the app its dir")
	}
}

func TestModulesHandler(t *testing.T) {
	p := testTree(t)
	state := &moduleState{p: p, gh: github{org: "acme"}, at: time.Now(),
		cached: map[string]*module{"widget:clock": {Kind: kindWidget, ID: "clock", Name: "Clock", Repo: "widgets", Requires: []string{}, Bundles: []string{}}}}
	srv := httptest.NewServer(modulesHandler(p, state, "s3cret"))
	defer srv.Close()

	call := func(method, path, body, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("X-Modules-Token", token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	if code, _ := call("GET", "/", "", ""); code != 200 {
		t.Errorf("page: %d", code)
	}
	if res, err := http.Get(srv.URL + "/fonts/inter-latin.woff2"); err != nil || res.StatusCode != 200 {
		t.Errorf("embedded font not served: %v %v", err, res)
	}
	if code, _ := call("GET", "/api/modules", "", "wrong"); code != 401 {
		t.Errorf("API must require the token, got %d", code)
	}
	if code, out := call("GET", "/api/modules", "", "s3cret"); code != 200 || len(out["modules"].([]any)) != 1 {
		t.Errorf("modules: %d %v", code, out)
	}
	// A run with nothing to change and no apply finishes cleanly — and a second
	// run can start after it (the job state resets without clobbering its lock).
	for i := 0; i < 2; i++ {
		if code, _ := call("POST", "/api/run", `{"install":[],"remove":[],"mode":"none"}`, "s3cret"); code != 200 {
			t.Fatalf("run %d: %d", i, code)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, j := call("GET", "/api/job", "", "s3cret")
			if j["running"] == false {
				if j["ok"] != true {
					t.Fatalf("run %d failed: %v", i, j["log"])
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("run never finished")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestPlanRuntimeCascadeAndWidgetHostExtras(t *testing.T) {
	mods := catalogFixture()
	mods["app:pulse"] = &module{Kind: kindApp, ID: "pulse", Name: "Pulse", Repo: "pulse", Hosts: kindWidget, Requires: []string{}, Bundles: []string{}}
	for _, k := range []string{"app:pulse", "app:echo", "widget:echo", "widget:core", "widget:sysinfo", "widget:sys-load",
		"app:dex", "plugin:binders", "service:plugin-runtime"} {
		mods[k].Installed = true
	}

	pl := planModules(mods, nil, []string{"plugin-runtime"}, planOpts{})
	if len(pl.Errors) > 0 || !strings.Contains(strings.Join(keys(pl.Remove), " "), "plugin:binders") || len(pl.Warnings) == 0 {
		t.Errorf("removing the runtime must remove every plugin, with a warning: %v %v %v", keys(pl.Remove), pl.Warnings, pl.Errors)
	}

	pl = planModules(mods, nil, []string{"pulse"}, planOpts{})
	if len(pl.Remove) != 1 || pl.Extras == nil || pl.Extras.Applied {
		t.Fatalf("removing the widget host should offer (not apply) removing widgets: %v %+v", keys(pl.Remove), pl.Extras)
	}
	if got := strings.Join(pl.Extras.Modules, " "); got != "widget:sysinfo widget:sys-load" && got != "widget:sys-load widget:sysinfo" {
		t.Errorf("extras should be the standalone widgets, not echo's bundled one: %s", got)
	}
	pl = planModules(mods, nil, []string{"pulse"}, planOpts{Extras: true})
	if got := strings.Join(keys(pl.Remove), " "); !strings.Contains(got, "widget:sys-load") || strings.Contains(got, "widget:echo") {
		t.Errorf("accepted extras remove standalone widgets only: %s", got)
	}

	all := planModules(mods, nil, allInstalled(mods), planOpts{})
	if len(all.Errors) > 0 {
		t.Fatalf("remove everything: %v", all.Errors)
	}
	for _, m := range mods {
		if m.Installed && !m.required && !strings.Contains(strings.Join(keys(all.Remove), " "), m.key()) {
			t.Errorf("remove everything left %s", m.key())
		}
	}
}

func TestSkeletonsAreNotModules(t *testing.T) {
	p := testTree(t)
	// What Docker leaves after a hub library is removed while a container still mounts it.
	os.MkdirAll(filepath.Join(p.apps, "pulse", "client"), 0o755)
	write(t, filepath.Join(p.apps, "pulse", ".DS_Store"), "x")
	if libs, _ := findHubLibraries(p); len(libs) != 0 {
		t.Errorf("an empty skeleton must not be a hub library: %v", libs)
	}
	if !emptyTree(filepath.Join(p.apps, "pulse")) {
		t.Error("a tree of empty folders (and .DS_Store) is empty")
	}
	write(t, filepath.Join(p.apps, "pulse", "client", "hub.js"), "x")
	if emptyTree(filepath.Join(p.apps, "pulse")) {
		t.Error("a tree with a real file isn't empty")
	}
}

func TestChangedDirs(t *testing.T) {
	p := testTree(t)
	pl := modulePlan{
		Install: []*module{{Kind: kindApp, ID: "pulse", Repo: "pulse"}, {Kind: kindWidget, ID: "clock"}},
		Remove:  []*module{{Kind: kindService, ID: "plugin-runtime", Repo: "plugin-runtime", localDir: filepath.Join(p.root, "plugin-runtime")}},
	}
	got := changedDirs(p, pl)
	want := []string{filepath.Join(p.apps, "pulse"), filepath.Join(p.root, "widgets"), filepath.Join(p.root, "plugin-runtime")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changedDirs = %v, want %v", got, want)
	}
}
