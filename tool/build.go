package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ── Colour helpers (match the bash script) ───────────────────────────────────

const (
	cBold   = "\033[1m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cDim    = "\033[2m"
	cReset  = "\033[0m"
)

func step(s string) { fmt.Printf("\n%s%s▶ %s%s\n", cBold, cGreen, s, cReset) }
func warn(s string) { fmt.Printf("%s⚠  %s%s\n", cYellow, s, cReset) }
func skip(s string) { fmt.Printf("  %s↳ %s%s\n", cDim, s, cReset) }

// buildConfig holds parsed flags for `nucleus build`.
type buildConfig struct {
	force bool
	jobs  int
}

func runBuild(p paths, args []string) error {
	cfg := buildConfig{jobs: defaultJobs()}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force", "-f":
			cfg.force = true
		case "-j":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					cfg.jobs = n
				}
				i++
			}
		}
	}
	if os.Getenv("FORCE_REBUILD") == "1" {
		cfg.force = true
	}
	if cfg.force {
		warn("Force rebuild — ignoring build cache")
	}

	if err := loadEnv(filepath.Join(p.infra, ".env")); err != nil {
		return err
	}

	cacheDir := filepath.Join(p.infra, ".build-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	b := &builder{p: p, cfg: cfg, cacheDir: cacheDir}

	// ── Plan the independent build units, wiring symlinks up front ─────────────
	units, err := b.planUnits()
	if err != nil {
		return err
	}

	// ── Build all units in parallel (bounded by -j) ────────────────────────────
	if err := b.runUnits(units); err != nil {
		return err
	}

	// ── Generate nginx + compose configs (cheap — always run) ──────────────────
	// `nucleus build` prepares artifacts only: frontends compiled to their dist
	// dirs and the blue/green nginx + compose configs regenerated. It does NOT
	// touch Docker — deployment (per-color image build, health-gated switch) is
	// owned by the shell orchestrator (infra/production), so a build never
	// disturbs the running stack.
	step("Generating infra configs")
	if err := runGenerate(p); err != nil {
		return err
	}

	fmt.Printf("\n%s%s✓ Build complete — frontends and configs are ready.%s\n", cBold, cGreen, cReset)
	fmt.Printf("  %sDeploy with infra/production (blue/green); rollback with infra/rollback.%s\n", cDim, cReset)
	return nil
}

func defaultJobs() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4 // npm/vite already parallelise internally; over-subscription hurts
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ── Build units ──────────────────────────────────────────────────────────────

// unit is one independently-buildable frontend (the hub, a standalone app
// client, or the widgets workspace). manager selects npm vs pnpm.
type unit struct {
	name    string   // cache key + display label (e.g. "hub", "app-orbit", "widgets")
	dir     string   // working directory for install/build
	fp      string   // source fingerprint
	manager string   // "npm" or "pnpm"
	lock    string   // lockfile name for the deps cache
	fpRoots []string // source roots fed to srcHash
	// distCheck reports whether build output already exists (skip-when-unchanged).
	distCheck func() bool
}

type builder struct {
	p        paths
	cfg      buildConfig
	cacheDir string
	mu       sync.Mutex // serialises flushing a finished unit's output
}

// planUnits creates symlinks and computes fingerprints for every build unit.
// Symlink setup is cheap and done sequentially; fingerprints are computed in
// parallel since hashing source trees dominates the planning cost.
func (b *builder) planUnits() ([]*unit, error) {
	p := b.p

	// 1. Hub — bundles widget + library-app sources directly, so its fingerprint
	//    covers hub, core, widgets and every library app (client/ w/o vite.config).
	forceSymlink(filepath.Join(p.root, "core"), filepath.Join(p.root, "hub", "core"))
	forceSymlink(filepath.Join(p.root, "widgets"), filepath.Join(p.root, "hub", "widgets"))

	hubRoots := []string{filepath.Join(p.root, "hub"), filepath.Join(p.root, "core"), filepath.Join(p.root, "widgets")}
	appDirs, err := sortedDirs(p.apps)
	if err != nil {
		return nil, err
	}
	for _, appID := range appDirs {
		appDir := filepath.Join(p.apps, appID)
		if isIgnored(appDir) {
			continue
		}
		clientDir := filepath.Join(appDir, "client")
		if exists(clientDir) && !exists(filepath.Join(clientDir, "vite.config.js")) {
			forceSymlink(clientDir, filepath.Join(p.root, "hub", appID))
			hubRoots = append(hubRoots, clientDir)
		}
	}

	units := []*unit{{
		name: "hub", dir: filepath.Join(p.root, "hub"), manager: "npm", lock: "package-lock.json",
		distCheck: distExists(filepath.Join(p.root, "hub", "dist")),
		fp:        "", // filled below
	}}
	hubRootsCopy := hubRoots

	// 2. Standalone app clients (apps/*/client with a vite.config.js).
	for _, appID := range appDirs {
		appDir := filepath.Join(p.apps, appID)
		clientDir := filepath.Join(appDir, "client")
		if isIgnored(appDir) || !exists(filepath.Join(clientDir, "vite.config.js")) {
			continue
		}
		forceSymlink(filepath.Join(p.root, "core"), filepath.Join(clientDir, "core"))
		roots := []string{clientDir, filepath.Join(p.root, "core")}
		if appID == "echo" {
			// Echo bundles every app's echo integration via import.meta.glob.
			forceSymlink(p.apps, filepath.Join(clientDir, "apps"))
			for _, other := range appDirs {
				echoDir := filepath.Join(p.apps, other, "echo")
				if exists(echoDir) {
					roots = append(roots, echoDir)
				}
			}
		}
		rootsCopy := roots
		u := &unit{
			name: "app-" + appID, dir: clientDir, manager: "npm", lock: "package-lock.json",
			distCheck: distExists(filepath.Join(clientDir, "dist")),
		}
		u.fpRoots = rootsCopy
		units = append(units, u)
	}

	// 3. Widgets (pnpm workspace).
	wUnit := &unit{
		name: "widgets", dir: p.widgets, manager: "pnpm", lock: "pnpm-lock.yaml",
		distCheck: func() bool { return globExists(filepath.Join(p.widgets, "*", "client", "dist")) },
	}
	wUnit.fpRoots = []string{p.widgets}
	units = append(units, wUnit)

	// Hub fingerprint roots.
	units[0].fpRoots = hubRootsCopy

	// Compute all fingerprints in parallel.
	var wg sync.WaitGroup
	errs := make([]error, len(units))
	for i, u := range units {
		wg.Add(1)
		go func(i int, u *unit) {
			defer wg.Done()
			fp, err := srcHash(b.p.root, u.fpRoots)
			if err != nil {
				errs[i] = err
				return
			}
			u.fp = fp
		}(i, u)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return units, nil
}

// runUnits builds the units concurrently, bounded by cfg.jobs. Each unit's
// output is buffered and flushed atomically so parallel logs stay readable.
func (b *builder) runUnits(units []*unit) error {
	sem := make(chan struct{}, b.cfg.jobs)
	var wg sync.WaitGroup
	errs := make([]error, len(units))
	for i, u := range units {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u *unit) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = b.buildUnit(u)
		}(i, u)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// buildUnit builds one unit when its fingerprint changed or its dist is missing.
// npm/pnpm install runs only when node_modules is absent or the lockfile changed.
// Mirrors build_unit() / the widgets block in the bash script.
func (b *builder) buildUnit(u *unit) error {
	out := &bytes.Buffer{}
	bcache := filepath.Join(b.cacheDir, u.name+".build")
	dcache := filepath.Join(b.cacheDir, u.name+".deps")

	if !b.cfg.force && u.distCheck() && readCache(bcache) == u.fp {
		b.flush(out, fmt.Sprintf("  %s↳ %s unchanged — skipping build%s\n", cDim, u.name, cReset))
		return nil
	}

	fmt.Fprintf(out, "\n%s%s▶ Building %s%s\n", cBold, cGreen, u.name, cReset)

	// deps: install only when node_modules is missing or the lockfile changed.
	lockPath := filepath.Join(u.dir, u.lock)
	if !exists(lockPath) {
		lockPath = filepath.Join(u.dir, "package.json")
	}
	lh, err := fileHash(lockPath)
	if err != nil {
		b.flush(out, "")
		return err
	}
	if !b.cfg.force && exists(filepath.Join(u.dir, "node_modules")) && readCache(dcache) == lh {
		fmt.Fprintf(out, "  %s↳ deps unchanged — skipping install%s\n", cDim, cReset)
	} else {
		if err := b.install(u, out); err != nil {
			b.flush(out, "")
			return fmt.Errorf("%s: install failed: %w", u.name, err)
		}
		writeCache(dcache, lh)
	}

	// build
	bld := exec.Command(u.manager, "run", "build")
	bld.Dir = u.dir
	bld.Stdout, bld.Stderr = out, out
	if err := bld.Run(); err != nil {
		b.flush(out, "")
		return fmt.Errorf("%s: build failed: %w", u.name, err)
	}
	writeCache(bcache, u.fp) // only on success — a failed build won't cache

	b.flush(out, "")
	return nil
}

// install runs the package manager's install step. npm prefers the clean,
// reproducible `ci`, falling back to `install`; pnpm uses the frozen lockfile.
func (b *builder) install(u *unit, out io.Writer) error {
	if u.manager == "pnpm" {
		c := exec.Command("pnpm", "install", "--frozen-lockfile")
		c.Dir, c.Stdout, c.Stderr = u.dir, out, out
		return c.Run()
	}
	// npm ci --prefer-offline (silence stderr), fall back to npm install.
	ci := exec.Command("npm", "ci", "--prefer-offline")
	ci.Dir, ci.Stdout = u.dir, out
	if err := ci.Run(); err != nil {
		inst := exec.Command("npm", "install")
		inst.Dir, inst.Stdout, inst.Stderr = u.dir, out, out
		return inst.Run()
	}
	return nil
}

// flush prints a finished unit's buffered output as one atomic block, optionally
// prefixed by a one-line message (used for the "unchanged" skip case).
func (b *builder) flush(out *bytes.Buffer, prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if prefix != "" {
		fmt.Print(prefix)
	}
	io.Copy(os.Stdout, out)
}

// ── Fingerprinting ───────────────────────────────────────────────────────────

// srcHash computes a deterministic content fingerprint over the given roots,
// excluding deps/outputs/vcs/logs and never following symlinks. The scheme is
// internal to the cache (no cross-tool contract), so it differs from the old
// bash sha1sum pipeline — the only effect is one full rebuild on first run.
func srcHash(root string, roots []string) (string, error) {
	type fileHashEntry struct {
		rel  string
		hash string
	}
	var paths []string
	for _, r := range roots {
		if !exists(r) {
			continue
		}
		err := filepath.WalkDir(r, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // tolerate transient/permission errors like find did
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "dist", ".git":
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() { // excludes symlinks, like find -type f
				return nil
			}
			if strings.HasSuffix(d.Name(), ".log") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)

	// Hash file contents in parallel.
	entries := make([]fileHashEntry, len(paths))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	var hashErr error
	var errMu sync.Mutex
	for i, path := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, path string) {
			defer wg.Done()
			defer func() { <-sem }()
			h, err := fileHash(path)
			if err != nil {
				errMu.Lock()
				hashErr = err
				errMu.Unlock()
				return
			}
			rel, _ := filepath.Rel(root, path)
			entries[i] = fileHashEntry{rel: filepath.ToSlash(rel), hash: h}
		}(i, path)
	}
	wg.Wait()
	if hashErr != nil {
		return "", hashErr
	}

	roll := sha256.New()
	for _, e := range entries {
		io.WriteString(roll, e.rel)
		roll.Write([]byte{0})
		io.WriteString(roll, e.hash)
		roll.Write([]byte{'\n'})
	}
	return hex.EncodeToString(roll.Sum(nil)), nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ── Small helpers ────────────────────────────────────────────────────────────

func readCache(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeCache(path, val string) {
	os.WriteFile(path, []byte(val+"\n"), 0o644)
}

func distExists(dir string) func() bool {
	return func() bool { return exists(dir) }
}

func globExists(pattern string) bool {
	matches, err := filepath.Glob(pattern)
	return err == nil && len(matches) > 0
}

func sortedDirs(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// forceSymlink creates (or replaces) a symlink at linkPath pointing to target,
// matching `ln -sfn`: it never dereferences an existing link-to-dir. The link is
// written RELATIVE to its own directory so it stays portable across checkouts
// (these links are committed) and a rebuild never rewrites a committed relative
// link into a machine-specific absolute path.
func forceSymlink(target, linkPath string) {
	if fi, err := os.Lstat(linkPath); err == nil {
		// Replace existing symlinks; leave real dirs/files alone (ln -sfn would
		// error on a real dir too — we mirror by only removing symlinks).
		if fi.Mode()&os.ModeSymlink != 0 {
			os.Remove(linkPath)
		}
	}
	if rel, err := filepath.Rel(filepath.Dir(linkPath), target); err == nil {
		target = rel
	}
	os.Symlink(target, linkPath)
}

// hasAVX reports whether the CPU advertises AVX (MongoDB 5+ requires it).
func hasAVX() bool {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.Contains(sc.Text(), " avx ") {
			return true
		}
	}
	return false
}

// loadEnv reads a KEY=VALUE .env file and exports each entry into the process
// environment so child npm/pnpm/docker processes inherit it (set -a; source).
func loadEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		os.Setenv(key, val)
	}
	return nil
}

// envFileValue reads a single KEY's value from a .env file without mutating the
// process environment. Returns "" if the file or key is absent.
func envFileValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 || strings.TrimSpace(line[:eq]) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
	}
	return ""
}
