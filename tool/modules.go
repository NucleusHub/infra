package main

// Module management — the engine behind `nucleus modules` (CLI) and
// `nucleus modules ui` (a small local web UI, see modules_cmd.go).
//
// It scans the GitHub org for installable modules, compares them with the
// checkout, and installs/removes them:
//
//   - an app is a repo with a nucleus.app.json at its root → cloned to apps/<repo>;
//   - a plugin/widget is a <id>/nucleus.{plugin,widget}.json folder inside a
//     collection repo → added to plugins/ or widgets/ as a sparse checkout, so
//     each one installs on its own;
//   - repos marked nucleus.ignore, or without a manifest (hub, core, infra, …),
//     are not modules.
//
// Nothing is named here beyond the directory conventions infra already uses:
// dependencies come from the manifests (a widget's dependsOn, a plugin's
// dependencies.{apps,plugins}), an app bundles the widget that shares its id, a
// collection's `locked` entries (widgets/core) always come along, and plugins
// need the plugin runtime. Applying a change is a normal stack rebuild — see
// applyModules.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type moduleKind string

const (
	kindApp     moduleKind = "app"
	kindPlugin  moduleKind = "plugin"
	kindWidget  moduleKind = "widget"
	kindService moduleKind = "service" // platform service a module needs (plugin-runtime)
)

// hubProviderFile marks an app as the hub's dashboard provider — the widget
// launcher (see hub/src/composables/useDashboardProvider.js). Pulse ships it.
const hubProviderFile = "client/hub.js"

// pluginRuntimeRepo is the one platform service with no manifest of its own;
// infra already knows it by this name (see optionalModules).
const pluginRuntimeRepo = "plugin-runtime"

// module is one installable unit, merged from the org catalog and the checkout.
type module struct {
	Kind        moduleKind `json:"kind"`
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Version     string     `json:"version"`          // available (remote) version
	Repo        string     `json:"repo,omitempty"`   // org repo it comes from
	Icon        string     `json:"icon,omitempty"`   // raw SVG
	Requires    []string   `json:"requires"`         // keys (kind:id) it cannot run without
	Bundles     []string   `json:"bundles"`          // keys installed/removed together with it
	Hidden      bool       `json:"hidden,omitempty"` // bundled/required/service — not listed on its own
	System      bool       `json:"system,omitempty"` // a locked app (e.g. the admin console)
	Hosts       moduleKind `json:"hosts,omitempty"`  // kind this app launches/manages (widget, for a dashboard provider)

	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	Local            bool   `json:"local,omitempty"` // installed, but in no org repo
	localDir         string // where it lives in the checkout
	required         bool   // a collection's locked entry
}

func (m *module) key() string { return string(m.Kind) + ":" + m.ID }

// applyLocked interprets a manifest's `locked`: in a collection it marks the
// base entry every other entry needs (widgets/core — installed with them, never
// listed on its own); on an app it only means a system app.
func (m *module) applyLocked(locked bool) {
	if !locked {
		return
	}
	if _, isColl := collectionDirs[m.Kind]; isColl {
		m.required, m.Hidden = true, true
	} else {
		m.System = true
	}
}

// manifestInfo is the part of any Nucleus manifest the installer reads.
type manifestInfo struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Version      string          `json:"version"`
	Icon         string          `json:"icon"`
	Locked       bool            `json:"locked"`
	DependsOn    json.RawMessage `json:"dependsOn"`
	Dependencies struct {
		Apps    map[string]string `json:"apps"`
		Plugins map[string]string `json:"plugins"`
	} `json:"dependencies"`
}

func (mi manifestInfo) dependsOn() []string {
	var one string
	if json.Unmarshal(mi.DependsOn, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	json.Unmarshal(mi.DependsOn, &many)
	return many
}

// requires lists the keys a module of this kind cannot run without.
func (mi manifestInfo) requires(kind moduleKind) []string {
	var out []string
	switch kind {
	case kindWidget:
		for _, id := range mi.dependsOn() {
			out = append(out, string(kindWidget)+":"+id)
		}
	case kindPlugin:
		out = append(out, string(kindService)+":"+pluginRuntimeRepo)
		for id := range mi.Dependencies.Plugins {
			out = append(out, string(kindPlugin)+":"+id)
		}
		for id := range mi.Dependencies.Apps {
			out = append(out, string(kindApp)+":"+id)
		}
	}
	sort.Strings(out)
	return out
}

// collectionDirs maps a collection kind to its folder in the checkout.
var collectionDirs = map[moduleKind]string{kindPlugin: "plugins", kindWidget: "widgets"}

var manifestFiles = map[moduleKind]string{
	kindApp:    "nucleus.app.json",
	kindPlugin: "nucleus.plugin.json",
	kindWidget: "nucleus.widget.json",
}

// repoLayout classifies a repo from its file list.
type repoLayout struct {
	app         bool                    // nucleus.app.json at the root
	collections map[moduleKind][]string // kind → entry dirs holding a manifest
}

func classifyRepo(files []string) (repoLayout, bool) {
	l := repoLayout{collections: map[moduleKind][]string{}}
	for _, f := range files {
		if f == "nucleus.ignore" {
			return repoLayout{}, false
		}
		if f == manifestFiles[kindApp] {
			l.app = true
		}
		for kind := range collectionDirs {
			if dir, file, ok := strings.Cut(f, "/"); ok && file == manifestFiles[kind] {
				l.collections[kind] = append(l.collections[kind], dir)
			}
		}
	}
	for _, dirs := range l.collections {
		sort.Strings(dirs)
	}
	return l, true
}

// ── GitHub ──────────────────────────────────────────────────────────────────

type github struct {
	org, token string
	client     *http.Client
}

var githubAPI = "https://api.github.com"

// errEmptyRepo is GitHub's 409 for a repository with no commits yet.
var errEmptyRepo = errors.New("empty repository")

func (g github) get(path string, raw bool) ([]byte, error) {
	req, err := http.NewRequest("GET", githubAPI+path, nil)
	if err != nil {
		return nil, err
	}
	if raw {
		req.Header.Set("Accept", "application/vnd.github.raw")
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusConflict {
		return nil, errEmptyRepo
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub %s: %s", path, res.Status)
	}
	return body, nil
}

type ghRepo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

func (g github) repos() ([]ghRepo, error) {
	var all []ghRepo
	base := fmt.Sprintf("/orgs/%s/repos?type=all&per_page=100", g.org)
	for page := 1; ; page++ {
		body, err := g.get(fmt.Sprintf("%s&page=%d", base, page), false)
		if err != nil && page == 1 {
			// Not an org — try it as a user account.
			base = fmt.Sprintf("/users/%s/repos?per_page=100", g.org)
			body, err = g.get(fmt.Sprintf("%s&page=%d", base, page), false)
		}
		if err != nil {
			return nil, err
		}
		var batch []ghRepo
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

func (g github) tree(repo, branch string) ([]string, error) {
	body, err := g.get(fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", g.org, repo, branch), false)
	if err != nil {
		return nil, err
	}
	var t struct {
		Tree []struct {
			Path string `json:"path"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	files := make([]string, len(t.Tree))
	for i, e := range t.Tree {
		files[i] = e.Path
	}
	return files, nil
}

func (g github) file(repo, branch, path string) ([]byte, error) {
	return g.get(fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", g.org, repo, path, branch), true)
}

// githubSettings resolves the org and a token, in order: infra/.env
// (NUCLEUS_GITHUB_ORG / NUCLEUS_GITHUB_TOKEN), the environment (also
// GITHUB_TOKEN), `gh auth token`, then the git credential helper — the same
// credentials the checkout already clones with. The org defaults to the one
// infra itself was cloned from.
func githubSettings(p paths) (org, token string) {
	env := func(k string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return envFileValue(filepath.Join(p.infra, ".env"), k)
	}
	org = env("NUCLEUS_GITHUB_ORG")
	if org == "" {
		org, _ = remoteOrg(gitOut(p.infra, "remote", "get-url", "origin"))
	}
	for _, k := range []string{"NUCLEUS_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if token = env(k); token != "" {
			return
		}
	}
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		if token = strings.TrimSpace(string(out)); token != "" {
			return
		}
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	if out, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(line, "password="); ok {
				token = strings.TrimSpace(v)
			}
		}
	}
	return
}

var remoteRe = regexp.MustCompile(`github\.com[:/]([^/]+)/`)

// remoteOrg extracts the owner from an https or ssh GitHub remote, and reports
// whether it was ssh (clones then use the same transport and keys).
func remoteOrg(url string) (org string, ssh bool) {
	m := remoteRe.FindStringSubmatch(url)
	if m == nil {
		return "", false
	}
	return m[1], strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
}

// ── Catalog ─────────────────────────────────────────────────────────────────

// fetchCatalog scans every repo in the org and returns the modules it offers,
// keyed by kind:id. Collection kinds also report which repo provides them.
func fetchCatalog(g github) (map[string]*module, map[moduleKind]string, []string, error) {
	repos, err := g.repos()
	if err != nil {
		return nil, nil, nil, err
	}
	var (
		mu          sync.Mutex
		wg          sync.WaitGroup
		sem         = make(chan struct{}, 8)
		mods        = map[string]*module{}
		collections = map[moduleKind]string{}
		errs        []string
	)
	add := func(m *module) {
		mu.Lock()
		defer mu.Unlock()
		if _, dup := mods[m.key()]; !dup {
			mods[m.key()] = m
		}
	}
	fetchManifest := func(repo ghRepo, kind moduleKind, dir string, files map[string]bool) {
		defer wg.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		prefix := ""
		if dir != "" {
			prefix = dir + "/"
		}
		body, err := g.file(repo.Name, repo.DefaultBranch, prefix+manifestFiles[kind])
		var mi manifestInfo
		if err == nil {
			err = json.Unmarshal(body, &mi)
		}
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s/%s: %v", repo.Name, prefix+manifestFiles[kind], err))
			mu.Unlock()
			return
		}
		id := mi.ID
		if id == "" {
			id = dir
			if id == "" {
				id = repo.Name
			}
		}
		m := &module{Kind: kind, ID: id, Name: mi.Name, Description: mi.Description, Version: mi.Version,
			Repo: repo.Name, Requires: mi.requires(kind), Bundles: []string{}}
		m.applyLocked(mi.Locked)
		if m.Name == "" {
			m.Name = id
		}
		icon := mi.Icon
		if icon == "" || !strings.HasSuffix(icon, ".svg") {
			icon = "icon.svg"
		}
		if kind == kindApp && files[prefix+hubProviderFile] {
			m.Hosts = kindWidget
		}
		if files[prefix+icon] {
			if svg, err := g.file(repo.Name, repo.DefaultBranch, prefix+icon); err == nil {
				m.Icon = string(svg)
			}
		}
		add(m)
	}

	for _, repo := range repos {
		if repo.Archived {
			continue
		}
		wg.Add(1)
		go func(repo ghRepo) {
			defer wg.Done()
			sem <- struct{}{}
			list, err := g.tree(repo.Name, repo.DefaultBranch)
			<-sem
			if errors.Is(err, errEmptyRepo) {
				return
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, err.Error())
				mu.Unlock()
				return
			}
			if repo.Name == pluginRuntimeRepo {
				add(&module{Kind: kindService, ID: pluginRuntimeRepo, Name: "Plugin runtime",
					Description: "Discovers installed plugins and serves the plugin registry (/api/plugins). Every plugin needs it.",
					Repo:        repo.Name, Requires: []string{}, Bundles: []string{}})
				return
			}
			layout, ok := classifyRepo(list)
			if !ok {
				return
			}
			files := map[string]bool{}
			for _, f := range list {
				files[f] = true
			}
			if layout.app {
				wg.Add(1)
				go fetchManifest(repo, kindApp, "", files)
			}
			for kind, dirs := range layout.collections {
				mu.Lock()
				if _, taken := collections[kind]; !taken || repo.Name == collectionDirs[kind] {
					collections[kind] = repo.Name
				}
				mu.Unlock()
				for _, dir := range dirs {
					wg.Add(1)
					go fetchManifest(repo, kind, dir, files)
				}
			}
		}(repo)
	}
	wg.Wait()

	// A collection kind is served by ONE repo (the checkout has one plugins/
	// and one widgets/ folder); drop entries other repos offer for it.
	for k, m := range mods {
		if repo, ok := collections[m.Kind]; ok && m.Repo != repo {
			delete(mods, k)
		}
	}
	linkBundles(mods)
	sort.Strings(errs)
	if len(errs) > 0 && len(mods) == 0 {
		return nil, nil, nil, errors.New(strings.Join(errs, "; "))
	}
	return mods, collections, errs, nil
}

// linkBundles makes an app carry the widget that shares its id (an app's own
// dashboard widget, e.g. echo), and makes every collection entry carry that
// collection's required (locked) entries.
func linkBundles(mods map[string]*module) {
	for _, m := range mods {
		if m.Kind != kindApp {
			continue
		}
		if w, ok := mods[string(kindWidget)+":"+m.ID]; ok {
			m.Bundles = append(m.Bundles, w.key())
			w.Hidden = true
		}
	}
	for _, m := range mods {
		if m.required {
			continue
		}
		for _, r := range mods {
			if r.required && r.Kind == m.Kind && r.key() != m.key() {
				m.Requires = append(m.Requires, r.key())
			}
		}
		sort.Strings(m.Requires)
	}
}

// ── Local state ─────────────────────────────────────────────────────────────

func readManifestFile(path string) (manifestInfo, bool) {
	var mi manifestInfo
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &mi) != nil {
		return mi, false
	}
	return mi, true
}

// installedModules scans the checkout.
func installedModules(p paths) map[string]*module {
	out := map[string]*module{}
	put := func(kind moduleKind, dir string, mi manifestInfo) {
		id := mi.ID
		if id == "" {
			id = filepath.Base(dir)
		}
		name := mi.Name
		if name == "" {
			name = id
		}
		m := &module{Kind: kind, ID: id, Name: name, Description: mi.Description, Version: mi.Version,
			Installed: true, InstalledVersion: mi.Version, localDir: dir,
			Requires: mi.requires(kind), Bundles: []string{}}
		m.applyLocked(mi.Locked)
		out[m.key()] = m
	}
	if names, _ := sortedDirs(p.apps); names != nil {
		for _, n := range names {
			dir := filepath.Join(p.apps, n)
			if isIgnored(dir) {
				continue
			}
			if mi, ok := readManifestFile(filepath.Join(dir, manifestFiles[kindApp])); ok {
				put(kindApp, dir, mi)
				m := out[string(kindApp)+":"+firstNonEmpty(mi.ID, n)]
				m.Repo = repoName(dir)
				if exists(filepath.Join(dir, hubProviderFile)) {
					m.Hosts = kindWidget
				}
			}
		}
	}
	for kind, folder := range collectionDirs {
		base := filepath.Join(p.root, folder)
		names, _ := sortedDirs(base)
		for _, n := range names {
			dir := filepath.Join(base, n)
			if mi, ok := readManifestFile(filepath.Join(dir, manifestFiles[kind])); ok {
				put(kind, dir, mi)
			}
		}
	}
	if exists(filepath.Join(p.root, pluginRuntimeRepo, "Dockerfile")) {
		out[string(kindService)+":"+pluginRuntimeRepo] = &module{Kind: kindService, ID: pluginRuntimeRepo,
			Name: "Plugin runtime", Installed: true, localDir: filepath.Join(p.root, pluginRuntimeRepo),
			Requires: []string{}, Bundles: []string{}}
	}
	return out
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// repoName is the org repo a clone came from (its origin), else its dir name.
func repoName(dir string) string {
	url := gitOut(dir, "remote", "get-url", "origin")
	if url == "" {
		return filepath.Base(dir)
	}
	return strings.TrimSuffix(filepath.Base(strings.TrimSuffix(url, "/")), ".git")
}

func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// mergeModules overlays the checkout onto the catalog. Anything installed that
// no repo offers is kept as Local (a custom module — never removed by us).
func mergeModules(catalog, local map[string]*module) map[string]*module {
	out := map[string]*module{}
	for k, m := range catalog {
		c := *m
		out[k] = &c
	}
	for k, l := range local {
		if m, ok := out[k]; ok {
			m.Installed, m.InstalledVersion, m.localDir = true, l.InstalledVersion, l.localDir
			if m.Hosts == "" {
				m.Hosts = l.Hosts
			}
			continue
		}
		l.Local = true
		out[k] = l
	}
	return out
}

// ── Planning ────────────────────────────────────────────────────────────────

type modulePlan struct {
	Install  []*module `json:"install"`
	Remove   []*module `json:"remove"`
	Notes    []string  `json:"notes"`
	Warnings []string  `json:"warnings"` // things that go that weren't asked for by name
	Errors   []string  `json:"errors"`
	// Extras is an opt-in follow-up removal the plan suggests — e.g. every
	// widget, when the last widget host goes. Accepted with planOpts.Extras.
	Extras *planExtras `json:"extras,omitempty"`
}

type planExtras struct {
	Label   string   `json:"label"`
	Modules []string `json:"modules"`
	Applied bool     `json:"applied"`
}

type planOpts struct {
	Extras bool // also remove what the plan offers in Extras
}

func (pl modulePlan) empty() bool { return len(pl.Install) == 0 && len(pl.Remove) == 0 }

// resolveKey finds a module from "id" or "kind:id" among the listed modules.
func resolveKey(mods map[string]*module, ref string) (*module, error) {
	if m, ok := mods[ref]; ok {
		return m, nil
	}
	var hits []*module
	for _, m := range mods {
		if m.ID == ref && !m.Hidden {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("no module %q", ref)
	}
	var keys []string
	for _, h := range hits {
		keys = append(keys, h.key())
	}
	sort.Strings(keys)
	return nil, fmt.Errorf("%q is ambiguous — use one of: %s", ref, strings.Join(keys, ", "))
}

// planModules expands the requested installs (with everything they require or
// bundle) and removals. A removal takes along what it bundles and — with a
// warning — every installed module that needs it (removing the plugin runtime
// removes the plugins). Removing the last widget host (an app that provides the
// hub dashboard) offers, as Extras, to remove every widget too. Nothing that
// isn't in any repo (Local) is ever removed; if it would have to be, the plan
// fails instead.
func planModules(mods map[string]*module, install, remove []string, opts planOpts) modulePlan {
	pl := modulePlan{Install: []*module{}, Remove: []*module{}, Notes: []string{}, Warnings: []string{}, Errors: []string{}}
	removing := map[string]bool{}
	installing := map[string]bool{}
	stays := func(m *module) bool { return (m.Installed && !removing[m.key()]) || installing[m.key()] }

	var addRemove func(m *module, why string, warn bool)
	addRemove = func(m *module, why string, warn bool) {
		if removing[m.key()] {
			return
		}
		switch {
		case !m.Installed:
			pl.Errors = append(pl.Errors, fmt.Sprintf("%s is not installed", m.key()))
			return
		case m.Local:
			if why == "" {
				pl.Errors = append(pl.Errors, fmt.Sprintf("%s isn't from any repo — remove it by hand if you're sure", m.key()))
			} else {
				pl.Errors = append(pl.Errors, fmt.Sprintf("%s would have to go too (%s), but it isn't from any repo — remove it by hand first", m.key(), why))
			}
			return
		case m.required && why == "":
			pl.Errors = append(pl.Errors, fmt.Sprintf("%s is required by its collection", m.key()))
			return
		}
		removing[m.key()] = true
		pl.Remove = append(pl.Remove, m)
		switch {
		case why != "" && warn:
			pl.Warnings = append(pl.Warnings, fmt.Sprintf("also removes %s (%s)", m.key(), why))
		case why != "":
			pl.Notes = append(pl.Notes, fmt.Sprintf("also removes %s (%s)", m.key(), why))
		}
		for _, b := range m.Bundles {
			if bm, ok := mods[b]; ok && bm.Installed {
				addRemove(bm, "bundled with "+m.key(), false)
			}
		}
	}
	for _, ref := range remove {
		m, err := resolveKey(mods, ref)
		if err != nil {
			pl.Errors = append(pl.Errors, err.Error())
			continue
		}
		addRemove(m, "", false)
	}

	var addInstall func(m *module, why string)
	addInstall = func(m *module, why string) {
		if installing[m.key()] || (m.Installed && !removing[m.key()]) {
			return
		}
		if m.Local || m.Repo == "" {
			pl.Errors = append(pl.Errors, fmt.Sprintf("%s isn't available in any repo", m.key()))
			return
		}
		installing[m.key()] = true
		for _, r := range append(append([]string{}, m.Requires...), m.Bundles...) {
			dep, ok := mods[r]
			if !ok {
				pl.Errors = append(pl.Errors, fmt.Sprintf("%s needs %s, which no repo offers", m.key(), r))
				continue
			}
			if removing[r] {
				pl.Errors = append(pl.Errors, fmt.Sprintf("%s needs %s, which is being removed", m.key(), r))
				continue
			}
			addInstall(dep, "needed by "+m.key())
		}
		pl.Install = append(pl.Install, m) // dependencies first
		if why != "" {
			pl.Notes = append(pl.Notes, fmt.Sprintf("also installs %s (%s)", m.key(), why))
		}
	}
	for _, ref := range install {
		m, err := resolveKey(mods, ref)
		if err != nil {
			pl.Errors = append(pl.Errors, err.Error())
			continue
		}
		if removing[m.key()] {
			pl.Errors = append(pl.Errors, fmt.Sprintf("%s is both installed and removed", m.key()))
			continue
		}
		if m.Installed {
			pl.Notes = append(pl.Notes, fmt.Sprintf("%s is already installed", m.key()))
			continue
		}
		addInstall(m, "")
	}

	// Removing the last host of a kind (the widget launcher) makes that kind
	// optional baggage: offer to remove all of it. Widgets an app that stays
	// bundles belong to that app, so they're kept.
	for _, m := range sortedModules(mods) {
		if !removing[m.key()] || m.Hosts == "" {
			continue
		}
		otherHost := false
		for _, h := range mods {
			if h.Hosts == m.Hosts && stays(h) {
				otherHost = true
			}
		}
		if otherHost {
			continue
		}
		keptByApp := map[string]bool{}
		for _, a := range mods {
			if stays(a) {
				for _, b := range a.Bundles {
					keptByApp[b] = true
				}
			}
		}
		var extra []*module
		for _, x := range sortedModules(mods) {
			if x.Kind == m.Hosts && x.Installed && !x.Local && !x.required && !removing[x.key()] && !keptByApp[x.key()] {
				extra = append(extra, x)
			}
		}
		if len(extra) == 0 {
			continue
		}
		pl.Extras = &planExtras{Label: fmt.Sprintf("Also remove all %d %ss — %s manages them", len(extra), m.Hosts, m.Name), Modules: keys(extra), Applied: opts.Extras}
		if opts.Extras {
			for _, x := range extra {
				addRemove(x, "you chose to remove all "+string(m.Hosts)+"s", false)
			}
		}
		break
	}

	// Whatever stays installed but needs something being removed goes too.
	for changed := true; changed; {
		changed = false
		for _, m := range sortedModules(mods) {
			if !stays(m) || installing[m.key()] {
				continue
			}
			for _, r := range m.Requires {
				if removing[r] && !installing[r] {
					before := len(pl.Remove)
					addRemove(m, "it needs "+r, true)
					changed = changed || len(pl.Remove) > before
					break
				}
			}
		}
	}

	// A platform service goes once nothing that stays installed needs it.
	if len(pl.Remove) > 0 {
		needed := map[string]bool{}
		for _, m := range mods {
			if stays(m) {
				for _, r := range m.Requires {
					needed[r] = true
				}
			}
		}
		for _, m := range sortedModules(mods) {
			if m.Kind == kindService && m.Installed && !m.Local && !needed[m.key()] && !removing[m.key()] {
				addRemove(m, "nothing needs it any more", false)
			}
		}
	}
	sort.Strings(pl.Errors)
	return pl
}

// allInstalled lists every installed module a "remove all" may take, limited
// to the given kinds (all kinds when none are given).
func allInstalled(mods map[string]*module, kinds ...moduleKind) []string {
	want := map[moduleKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []string
	for _, m := range sortedModules(mods) {
		if m.Installed && !m.Local && !m.Hidden && (len(kinds) == 0 || want[m.Kind]) {
			out = append(out, m.key())
		}
	}
	return out
}

func keys(ms []*module) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.key())
	}
	return out
}

// ── Executing ───────────────────────────────────────────────────────────────

// gitRunner runs git with the org credentials supplied through the
// environment (GIT_CONFIG_*), never on the command line or in .git/config.
type gitRunner struct {
	org, token string
	ssh        bool
	out        io.Writer
	base       string // overrides the GitHub URL (tests clone local repos)
}

func (g gitRunner) url(repo string) string {
	if g.base != "" {
		return g.base + "/" + repo
	}
	if g.ssh {
		return fmt.Sprintf("git@github.com:%s/%s.git", g.org, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", g.org, repo)
}

func (g gitRunner) run(dir string, args ...string) error {
	fmt.Fprintf(g.out, "$ git %s\n", strings.Join(args, " "))
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = g.out, g.out
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if g.token != "" && !g.ssh {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth)
	}
	return cmd.Run()
}

// deletable reports why a checkout must not be deleted: uncommitted/untracked
// work or commits no remote has. Ignored files (node_modules, dist, .env) are
// listed separately so the plan can say they'll go too.
func deletable(dir string) (ignored []string, err error) {
	if st := realChanges(gitOut(dir, "status", "--porcelain")); st != "" {
		return nil, fmt.Errorf("%s has uncommitted changes:\n%s", dir, st)
	}
	if un := gitOut(dir, "log", "--branches", "--not", "--remotes", "--oneline"); un != "" {
		return nil, fmt.Errorf("%s has commits that aren't pushed:\n%s", dir, un)
	}
	for _, line := range strings.Split(gitOut(dir, "status", "--porcelain", "--ignored"), "\n") {
		f, ok := strings.CutPrefix(line, "!! ")
		if !ok {
			continue
		}
		base := filepath.Base(strings.TrimSuffix(f, "/"))
		if base == "node_modules" || base == "dist" || base == ".DS_Store" {
			continue
		}
		ignored = append(ignored, f)
	}
	return ignored, nil
}

// emptyTree reports whether dir holds no files at all — only (nested) empty
// directories or OS litter. That's what Docker leaves behind when it recreates a
// removed module's bind-mount source (e.g. apps/pulse/client), so it's safe to
// clear rather than a reason to refuse.
func emptyTree(dir string) bool {
	empty := true
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || (!d.IsDir() && d.Name() != ".DS_Store") {
			empty = false
			return filepath.SkipAll
		}
		return nil
	})
	return empty
}

// pruneSkeletons removes what Docker recreated for modules a plan removed.
func pruneSkeletons(p paths, pl modulePlan, out io.Writer) {
	seen := map[string]bool{}
	for _, m := range pl.Remove {
		dir := m.localDir
		if _, isColl := collectionDirs[m.Kind]; isColl {
			dir = filepath.Join(p.root, collectionDirs[m.Kind]) // only if the whole collection went
		}
		if dir == "" || seen[dir] || !exists(dir) || exists(filepath.Join(dir, ".git")) || !emptyTree(dir) {
			continue
		}
		seen[dir] = true
		fmt.Fprintf(out, "▶ Cleaning up %s (empty folders Docker left behind)\n", dir)
		os.RemoveAll(dir)
	}
}

// realChanges drops OS litter (Finder's .DS_Store) from `git status --porcelain`
// output — it isn't anyone's work and shouldn't block a removal.
func realChanges(status string) string {
	var keep []string
	for _, line := range strings.Split(status, "\n") {
		if line != "" && filepath.Base(strings.TrimSpace(line[min(3, len(line)):])) != ".DS_Store" {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// checkRemovals verifies every removal is safe before anything is touched and
// adds a note for ignored files that will be deleted with it.
func checkRemovals(p paths, pl *modulePlan) {
	for _, m := range pl.Remove {
		var err error
		var ignored []string
		switch m.Kind {
		case kindApp, kindService:
			ignored, err = deletable(m.localDir)
		default:
			repo := filepath.Dir(m.localDir)
			rel, _ := filepath.Rel(repo, m.localDir)
			if st := realChanges(gitOut(repo, "status", "--porcelain", "--", rel)); st != "" {
				err = fmt.Errorf("%s has uncommitted changes:\n%s", m.localDir, st)
			}
		}
		if err != nil {
			pl.Errors = append(pl.Errors, err.Error())
		}
		if len(ignored) > 0 {
			pl.Notes = append(pl.Notes, fmt.Sprintf("removing %s also deletes ignored files: %s", m.key(), strings.Join(ignored, ", ")))
		}
	}
}

// executePlan applies a checked plan to the checkout. mods is the merged state
// the plan was made from.
func executePlan(p paths, g gitRunner, mods map[string]*module, pl modulePlan) error {
	if len(pl.Errors) > 0 {
		return errors.New(strings.Join(pl.Errors, "\n"))
	}
	// Collections: final set of entries per kind after the plan.
	want := map[moduleKind]map[string]bool{}
	touched := map[moduleKind]bool{}
	for _, m := range mods {
		// Local entries (custom, in no repo) are kept: dropping them from the
		// sparse set would hide committed-but-unpushed work.
		if _, isColl := collectionDirs[m.Kind]; isColl && m.Installed {
			if want[m.Kind] == nil {
				want[m.Kind] = map[string]bool{}
			}
			want[m.Kind][filepath.Base(m.localDir)] = true
		}
	}

	for _, m := range pl.Remove {
		switch m.Kind {
		case kindApp, kindService:
			fmt.Fprintf(g.out, "▶ Removing %s (%s)\n", m.key(), m.localDir)
			if err := os.RemoveAll(m.localDir); err != nil {
				return err
			}
		default:
			delete(want[m.Kind], filepath.Base(m.localDir))
			touched[m.Kind] = true
		}
	}
	for _, m := range pl.Install {
		switch m.Kind {
		case kindApp, kindService:
			dest := filepath.Join(p.apps, m.Repo)
			if m.Kind == kindService {
				dest = filepath.Join(p.root, m.Repo)
			}
			if exists(dest) {
				if !emptyTree(dest) {
					return fmt.Errorf("%s already exists — not overwriting it", dest)
				}
				fmt.Fprintf(g.out, "▶ Clearing %s (empty folders left behind)\n", dest)
				if err := os.RemoveAll(dest); err != nil {
					return err
				}
			}
			fmt.Fprintf(g.out, "▶ Installing %s\n", m.key())
			os.MkdirAll(filepath.Dir(dest), 0o755)
			if err := g.run(filepath.Dir(dest), "clone", g.url(m.Repo), dest); err != nil {
				return fmt.Errorf("clone %s: %w", m.Repo, err)
			}
		default:
			if want[m.Kind] == nil {
				want[m.Kind] = map[string]bool{}
			}
			want[m.Kind][m.ID] = true
			touched[m.Kind] = true
		}
	}

	for kind := range touched {
		if err := syncCollection(p, g, mods, kind, want[kind]); err != nil {
			return err
		}
	}
	return nil
}

// syncCollection makes plugins/ or widgets/ hold exactly the wanted entries
// (plus the collection's required ones) as a sparse checkout, cloning it
// sparsely first if needed and deleting it once nothing optional is left.
func syncCollection(p paths, g gitRunner, mods map[string]*module, kind moduleKind, want map[string]bool) error {
	dir := filepath.Join(p.root, collectionDirs[kind])
	var repo string
	ids := map[string]bool{}
	for id := range want {
		if m := mods[string(kind)+":"+id]; m == nil || !m.required {
			ids[id] = true
		}
	}
	optional := len(ids)
	for _, m := range mods {
		if m.Kind != kind {
			continue
		}
		if m.Repo != "" && !m.Local {
			repo = m.Repo
		}
		if m.required && optional > 0 {
			ids[m.ID] = true
		}
	}
	isRepo := exists(filepath.Join(dir, ".git"))

	if optional == 0 {
		if !isRepo {
			return nil
		}
		fmt.Fprintf(g.out, "▶ Removing %s/ (nothing left installed)\n", collectionDirs[kind])
		if _, err := deletable(dir); err != nil {
			return err
		}
		return os.RemoveAll(dir)
	}

	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Strings(list)
	fmt.Fprintf(g.out, "▶ %s/ → %s\n", collectionDirs[kind], strings.Join(list, ", "))
	if !isRepo {
		if exists(dir) {
			// Empty folders Docker recreated for a bind mount — safe to replace.
			if !emptyTree(dir) {
				return fmt.Errorf("%s exists and isn't a git checkout — not overwriting it", dir)
			}
			os.RemoveAll(dir)
		}
		if repo == "" {
			return fmt.Errorf("no repo provides %ss", kind)
		}
		if err := g.run(p.root, "clone", "--filter=blob:none", "--sparse", g.url(repo), dir); err != nil {
			return fmt.Errorf("clone %s: %w", repo, err)
		}
	}
	return g.run(dir, append([]string{"sparse-checkout", "set", "--cone", "--"}, list...)...)
}

// ── Applying ────────────────────────────────────────────────────────────────

type applyMode string

const (
	applyProduction applyMode = "production" // infra/production — blue/green, zero downtime
	applyDev        applyMode = "dev"        // infra/nucleus up — the dev stack
	applyNone       applyMode = "none"
)

func parseApplyMode(s string) (applyMode, error) {
	switch applyMode(s) {
	case "", applyProduction, "prod":
		return applyProduction, nil
	case applyDev, applyNone:
		return applyMode(s), nil
	}
	return "", fmt.Errorf("unknown apply mode %q (production, dev or none)", s)
}

// applyModules rebuilds the stack so the checkout's modules take effect: the
// generator re-derives nginx/compose, the builds pick up new sources, and
// removed modules' containers go away as orphans.
func applyModules(p paths, mode applyMode, out io.Writer) error {
	var cmd *exec.Cmd
	switch mode {
	case applyNone:
		return nil
	case applyDev:
		cmd = exec.Command(filepath.Join(p.infra, "nucleus"), "up", "-d", "--build", "--remove-orphans")
	default:
		cmd = exec.Command(filepath.Join(p.infra, "production"))
	}
	fmt.Fprintf(out, "\n▶ Applying (%s): %s\n", mode, strings.Join(cmd.Args, " "))
	cmd.Dir = p.infra
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// changedDirs lists the checkout folders a plan created, replaced or deleted.
func changedDirs(p paths, pl modulePlan) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, list := range [][]*module{pl.Install, pl.Remove} {
		for _, m := range list {
			switch m.Kind {
			case kindApp, kindService:
				if m.localDir != "" {
					add(m.localDir)
				} else if m.Kind == kindApp {
					add(filepath.Join(p.apps, m.Repo))
				} else {
					add(filepath.Join(p.root, m.Repo))
				}
			default:
				add(filepath.Join(p.root, collectionDirs[m.Kind]))
			}
		}
	}
	return out
}

// recreateStaleMounts force-recreates running dev containers that bind-mount a
// folder the plan replaced or deleted. Docker keeps such a mount pointing at
// the old directory (a fresh clone at the same path is invisible to it), and
// `up` alone won't recreate a service whose config didn't change.
func recreateStaleMounts(p paths, dirs []string, out io.Writer) error {
	if len(dirs) == 0 {
		return nil
	}
	ids, err := exec.Command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=nucleus").Output()
	if err != nil || len(strings.TrimSpace(string(ids))) == 0 {
		return nil
	}
	args := append([]string{"inspect", "-f", `{{index .Config.Labels "com.docker.compose.service"}}|{{range .Mounts}}{{.Source}};{{end}}`}, strings.Fields(string(ids))...)
	info, err := exec.Command("docker", args...).Output()
	if err != nil {
		return nil
	}
	var services []string
	for _, line := range strings.Split(strings.TrimSpace(string(info)), "\n") {
		svc, mounts, _ := strings.Cut(line, "|")
		for _, src := range strings.Split(mounts, ";") {
			stale := false
			for _, d := range dirs {
				if src == d || strings.HasPrefix(src, d+string(filepath.Separator)) {
					stale = true
				}
			}
			if stale && svc != "" {
				services = append(services, svc)
				break
			}
		}
	}
	if len(services) == 0 {
		return nil
	}
	sort.Strings(services)
	fmt.Fprintf(out, "\n▶ Recreating %s (they mounted changed module folders)\n", strings.Join(services, ", "))
	cmd := exec.Command(filepath.Join(p.infra, "nucleus"), append([]string{"up", "-d", "--force-recreate", "--no-deps"}, services...)...)
	cmd.Dir = p.infra
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// runningStacks reports which Nucleus stacks have containers up: the dev stack
// (compose project "nucleus") and/or a blue/green production color.
func runningStacks() (dev, production bool) {
	out, err := exec.Command("docker", "ps", "--format", `{{.Label "com.docker.compose.project"}}`).Output()
	if err != nil {
		return false, false
	}
	for _, proj := range strings.Fields(string(out)) {
		switch {
		case proj == "nucleus":
			dev = true
		case strings.HasPrefix(proj, "nucleus-") && proj != "nucleus-data" && proj != "nucleus-edge":
			production = true
		}
	}
	return
}

// stackHint warns when the chosen apply mode won't touch the stack that's
// actually running (e.g. Production on a laptop running the dev stack).
func stackHint(mode applyMode) string {
	dev, prod := runningStacks()
	switch {
	case mode == applyProduction && dev && !prod:
		return "only the dev stack is running — Production builds the blue/green stack and won't update it (use --dev)"
	case mode == applyDev && prod && !dev:
		return "only the production stack is running — Dev starts a separate dev stack and won't update it"
	}
	return ""
}

// ── State (catalog + checkout), cached for the UI ───────────────────────────

type moduleState struct {
	p      paths
	mu     sync.Mutex
	cached map[string]*module
	colls  map[moduleKind]string
	warns  []string // repos that couldn't be read in the last scan
	at     time.Time
	gh     github
	git    gitRunner
}

func newModuleState(p paths) *moduleState {
	org, token := githubSettings(p)
	_, ssh := remoteOrg(gitOut(p.infra, "remote", "get-url", "origin"))
	return &moduleState{p: p,
		gh:  github{org: org, token: token, client: &http.Client{Timeout: 20 * time.Second}},
		git: gitRunner{org: org, token: token, ssh: ssh}}
}

// modules returns the merged catalog + checkout. The remote catalog is cached
// for a few minutes; the checkout is always read fresh.
func (s *moduleState) modules(refresh bool) (map[string]*module, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gh.org == "" {
		return nil, errors.New("can't tell which GitHub org to scan — set NUCLEUS_GITHUB_ORG in infra/.env")
	}
	if refresh || s.cached == nil || time.Since(s.at) > 5*time.Minute {
		mods, colls, warns, err := fetchCatalog(s.gh)
		if err != nil {
			hint := ""
			if s.gh.token == "" {
				hint = " (no GitHub token found — set NUCLEUS_GITHUB_TOKEN in infra/.env for private repos)"
			}
			return nil, fmt.Errorf("scanning %s: %w%s", s.gh.org, err, hint)
		}
		s.cached, s.colls, s.warns, s.at = mods, colls, warns, time.Now()
	}
	return mergeModules(s.cached, installedModules(s.p)), nil
}

// warnings lists repos the last scan couldn't read.
func (s *moduleState) warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.warns...)
}

// sortedModules lists modules for display: kind order, then name.
func sortedModules(mods map[string]*module) []*module {
	order := map[moduleKind]int{kindApp: 0, kindPlugin: 1, kindWidget: 2, kindService: 3}
	list := make([]*module, 0, len(mods))
	for _, m := range mods {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool {
		if order[list[i].Kind] != order[list[j].Kind] {
			return order[list[i].Kind] < order[list[j].Kind]
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	return list
}

// ansiRe strips terminal colours from command output shown in the UI.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]|\r")

func stripANSI(b []byte) []byte { return ansiRe.ReplaceAll(b, nil) }
