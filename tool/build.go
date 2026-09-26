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

const (
	cBold   = "\033[1m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cDim    = "\033[2m"
	cReset  = "\033[0m"
)

func step(s string) { fmt.Printf("\n%s%s▶ %s%s\n", cBold, cGreen, s, cReset) }
func warn(s string) { fmt.Printf("%s⚠  %s%s\n", cYellow, s, cReset) }
func skip(s string) { fmt.Printf("  %s↳ %s%s\n", cDim, s, cReset) }

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

	units, err := b.planUnits()
	if err != nil {
		return err
	}

	if err := b.runUnits(units); err != nil {
		return err
	}

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

type unit struct {
	name      string
	dir       string
	fp        string
	manager   string
	lock      string
	fpRoots   []string
	distCheck func() bool
}

type builder struct {
	p        paths
	cfg      buildConfig
	cacheDir string
	mu       sync.Mutex
}

func (b *builder) planUnits() ([]*unit, error) {
	p := b.p

	optionalRoots := []string{filepath.Join(p.root, "plugins"), p.widgets}

	forceSymlink(filepath.Join(p.root, "core"), filepath.Join(p.root, "hub", "core"))
	forceSymlink(filepath.Join(p.root, "widgets"), filepath.Join(p.root, "hub", "widgets"))
	forceSymlink(filepath.Join(p.root, "plugins"), filepath.Join(p.root, "hub", "plugins"))

	hubRoots := append([]string{filepath.Join(p.root, "hub"), filepath.Join(p.root, "core")}, optionalRoots...)
	hubLibs, err := findHubLibraries(p)
	if err != nil {
		return nil, err
	}
	syncHubLibLinks(p, hubLibs)
	for _, l := range hubLibs {
		hubRoots = append(hubRoots, filepath.Join(p.apps, l.id, "client"))
	}
	appDirs, err := sortedDirs(p.apps)
	if err != nil {
		return nil, err
	}

	units := []*unit{{
		name: "hub", dir: filepath.Join(p.root, "hub"), manager: "npm", lock: "package-lock.json",
		distCheck: distExists(filepath.Join(p.root, "hub", "dist")),
		fp:        "",
	}}
	hubRootsCopy := hubRoots

	for _, appID := range appDirs {
		appDir := filepath.Join(p.apps, appID)
		clientDir := filepath.Join(appDir, "client")
		if isIgnored(appDir) || !isApp(appDir) || !exists(filepath.Join(clientDir, "vite.config.js")) {
			continue
		}
		forceSymlink(filepath.Join(p.root, "core"), filepath.Join(clientDir, "core"))
		roots := append([]string{clientDir, filepath.Join(p.root, "core")}, optionalRoots...)
		if appID == "echo" {
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

	if exists(filepath.Join(p.widgets, "package.json")) {
		wUnit := &unit{
			name: "widgets", dir: p.widgets, manager: "pnpm", lock: "pnpm-lock.yaml",
			distCheck: func() bool { return globExists(filepath.Join(p.widgets, "*", "client", "dist")) },
		}
		wUnit.fpRoots = []string{p.widgets}
		units = append(units, wUnit)
	}

	units[0].fpRoots = hubRootsCopy

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

func (b *builder) buildUnit(u *unit) error {
	out := &bytes.Buffer{}
	bcache := filepath.Join(b.cacheDir, u.name+".build")
	dcache := filepath.Join(b.cacheDir, u.name+".deps")

	if !b.cfg.force && u.distCheck() && readCache(bcache) == u.fp {
		b.flush(out, fmt.Sprintf("  %s↳ %s unchanged — skipping build%s\n", cDim, u.name, cReset))
		return nil
	}

	fmt.Fprintf(out, "\n%s%s▶ Building %s%s\n", cBold, cGreen, u.name, cReset)

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

func (b *builder) install(u *unit, out io.Writer) error {
	if u.manager == "pnpm" {
		c := exec.Command("pnpm", "install", "--frozen-lockfile")
		c.Dir, c.Stdout, c.Stderr = u.dir, out, out
		return c.Run()
	}
	ci := exec.Command("npm", "ci", "--prefer-offline")
	ci.Dir, ci.Stdout = u.dir, out
	if err := ci.Run(); err != nil {
		inst := exec.Command("npm", "install")
		inst.Dir, inst.Stdout, inst.Stderr = u.dir, out, out
		return inst.Run()
	}
	return nil
}

func (b *builder) flush(out *bytes.Buffer, prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if prefix != "" {
		fmt.Print(prefix)
	}
	io.Copy(os.Stdout, out)
}

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
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "dist", ".git":
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
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
	if os.IsNotExist(err) {
		return nil, nil
	}
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

// Relative targets keep committed links portable across checkouts.
func forceSymlink(target, linkPath string) {
	if fi, err := os.Lstat(linkPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			os.Remove(linkPath)
		}
	}
	if rel, err := filepath.Rel(filepath.Dir(linkPath), target); err == nil {
		target = rel
	}
	os.Symlink(target, linkPath)
}

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
