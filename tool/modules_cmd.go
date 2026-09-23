package main

// `nucleus modules` — the CLI and the local web UI over modules.go.
//
//	nucleus modules                         list what's installed and available
//	nucleus modules install <id>... [flags] install (with dependencies), then apply
//	nucleus modules remove  <id>... [flags] remove (with what depends on it), then apply
//	nucleus modules remove --all [kind...]  remove everything (or every app/plugin/widget/service)
//	nucleus modules apply [--dev]           rebuild the stack from the checkout
//	nucleus modules ui [--addr A]           the same, in a browser
//
// Flags: --dev applies with the dev stack instead of production, --no-apply
// only changes the checkout, --yes skips the confirmation prompt, --with-extras
// accepts the plan's optional follow-up (e.g. removing all widgets together
// with the widget launcher) without asking.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The UI is one page plus the self-hosted fonts it's set in (ui/fonts), all
// embedded so the binary serves it with no external requests.
//
//go:embed ui
var modulesUI embed.FS

func runModules(p paths, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("modules "+sub, flag.ContinueOnError)
	dev := fs.Bool("dev", false, "apply with the dev stack (infra/nucleus up) instead of infra/production")
	noApply := fs.Bool("no-apply", false, "change the checkout only; don't rebuild the stack")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	fs.BoolVar(yes, "y", false, "shorthand for --yes")
	refresh := fs.Bool("refresh", false, "rescan the org even if a recent scan is cached")
	addr := fs.String("addr", "127.0.0.1:7777", "ui: address to listen on")
	noOpen := fs.Bool("no-open", false, "ui: don't open a browser")
	all := fs.Bool("all", false, "remove: every installed module (optionally only the given kinds)")
	withExtras := fs.Bool("with-extras", false, "also do the plan's optional follow-up (e.g. remove all widgets with the widget launcher)")
	// Allow flags after positional ids (install a b --dev).
	var ids []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		ids, args = append(ids, args[0]), args[1:]
	}

	mode := applyProduction
	if *dev {
		mode = applyDev
	}
	if *noApply {
		mode = applyNone
	}
	state := newModuleState(p)

	switch sub {
	case "list", "ls":
		mods, err := state.modules(*refresh)
		if err != nil {
			return err
		}
		printModules(mods)
		for _, w := range state.warnings() {
			warn("couldn't read " + w)
		}
		return nil
	case "install", "add", "remove", "rm", "uninstall":
		installing := sub == "install" || sub == "add"
		if len(ids) == 0 && !(*all && !installing) {
			return fmt.Errorf("usage: modules %s <id>... [--dev|--no-apply] [--yes]   (remove also takes --all [apps|plugins|widgets|services])", sub)
		}
		mods, err := state.modules(*refresh)
		if err != nil {
			return err
		}
		if *all && !installing {
			var kinds []moduleKind
			for _, id := range ids {
				k, ok := kindNames[strings.ToLower(id)]
				if !ok {
					return fmt.Errorf("--all takes kinds (apps, plugins, widgets, services), not %q", id)
				}
				kinds = append(kinds, k)
			}
			ids = allInstalled(mods, kinds...)
			if len(ids) == 0 {
				fmt.Println("Nothing installed to remove.")
				return nil
			}
		}
		plan := func(extras bool) modulePlan {
			var pl modulePlan
			if installing {
				pl = planModules(mods, ids, nil, planOpts{Extras: extras})
			} else {
				pl = planModules(mods, nil, ids, planOpts{Extras: extras})
			}
			checkRemovals(p, &pl)
			return pl
		}
		pl := plan(*withExtras)
		printPlan(pl, mode)
		if len(pl.Errors) == 0 && !pl.empty() {
			if hint := stackHint(mode); hint != "" {
				warn(hint)
			}
		}
		if pl.Extras != nil && !pl.Extras.Applied && !*yes && len(pl.Errors) == 0 {
			if confirm(pl.Extras.Label + " (" + strings.Join(pl.Extras.Modules, ", ") + ")?") {
				pl = plan(true)
				fmt.Println()
				printPlan(pl, mode)
			}
		}
		if len(pl.Errors) > 0 {
			return errors.New("nothing changed")
		}
		if pl.empty() {
			return nil
		}
		if !*yes && !confirm("Proceed?") {
			return errors.New("cancelled")
		}
		state.git.out = os.Stdout
		if err := executePlan(p, state.git, mods, pl); err != nil {
			return err
		}
		return applyPlan(p, pl, mode, os.Stdout)
	case "apply":
		if mode == applyNone {
			return nil
		}
		return applyModules(p, mode, os.Stdout)
	case "ui":
		return serveModulesUI(p, state, *addr, !*noOpen)
	}
	return fmt.Errorf("unknown modules command %q (list, install, remove, apply, ui)", sub)
}

// kindNames maps what --all accepts to kinds.
var kindNames = map[string]moduleKind{
	"app": kindApp, "apps": kindApp, "plugin": kindPlugin, "plugins": kindPlugin,
	"widget": kindWidget, "widgets": kindWidget, "service": kindService, "services": kindService,
}

// applyPlan rebuilds the stack after a plan ran, then tidies what Docker got
// wrong about it: dev containers still mounting replaced/deleted folders are
// recreated, and folders Docker recreated for removed modules are deleted.
func applyPlan(p paths, pl modulePlan, mode applyMode, out io.Writer) error {
	err := applyModules(p, mode, out)
	if err == nil && mode == applyDev {
		err = recreateStaleMounts(p, changedDirs(p, pl), out)
	}
	pruneSkeletons(p, pl, out)
	return err
}

func printModules(mods map[string]*module) {
	titles := map[moduleKind]string{kindApp: "Apps", kindPlugin: "Plugins", kindWidget: "Widgets", kindService: "Services"}
	var last moduleKind
	for _, m := range sortedModules(mods) {
		if m.Hidden {
			continue
		}
		if m.Kind != last {
			fmt.Printf("\n%s%s%s\n", cBold, titles[m.Kind], cReset)
			last = m.Kind
		}
		mark, status := "○", ""
		switch {
		case m.Local:
			mark, status = "●", cDim+"local"+cReset
		case m.Installed && m.InstalledVersion != "" && m.Version != "" && m.InstalledVersion != m.Version:
			mark, status = "●", fmt.Sprintf("%sinstalled %s → %s available%s", cYellow, m.InstalledVersion, m.Version, cReset)
		case m.Installed:
			mark, status = cGreen+"●"+cReset, cGreen+"installed"+cReset
		}
		if m.System {
			status += cDim + " (system)" + cReset
		}
		fmt.Printf("  %s %-22s %-24s %-8s %s\n", mark, m.ID, truncate(m.Name, 24), m.Version, status)
	}
	fmt.Println()
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func printPlan(pl modulePlan, mode applyMode) {
	for _, m := range pl.Remove {
		fmt.Printf("  %s- remove%s  %s\n", cYellow, cReset, m.key())
	}
	for _, m := range pl.Install {
		fmt.Printf("  %s+ install%s %s\n", cGreen, cReset, m.key())
	}
	for _, n := range pl.Notes {
		fmt.Printf("  %s· %s%s\n", cDim, n, cReset)
	}
	for _, w := range pl.Warnings {
		fmt.Printf("  %s⚠ %s%s\n", cYellow, w, cReset)
	}
	if pl.Extras != nil && !pl.Extras.Applied {
		fmt.Printf("  %s· not removing: %s (pass --with-extras to include)%s\n", cDim, strings.Join(pl.Extras.Modules, ", "), cReset)
	}
	for _, e := range pl.Errors {
		fmt.Printf("  %s✗ %s%s\n", cRed, e, cReset)
	}
	if len(pl.Errors) == 0 && !pl.empty() {
		if mode == applyNone {
			fmt.Println("  then: nothing (--no-apply) — run `modules apply` later")
		} else {
			fmt.Printf("  then: apply with %s\n", mode)
		}
	}
}

func confirm(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// ── Web UI ──────────────────────────────────────────────────────────────────

// job is the one install/remove/apply run the UI can have in flight.
type job struct {
	mu      sync.Mutex
	running bool
	ok      bool
	done    bool
	title   string
	log     bytes.Buffer
}

func (j *job) Write(b []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log.Write(stripANSI(b))
	return len(b), nil
}

func (j *job) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{"running": j.running, "done": j.done, "ok": j.ok, "title": j.title, "log": j.log.String()}
}

type uiRequest struct {
	Install []string `json:"install"`
	Remove  []string `json:"remove"`
	Mode    string   `json:"mode"`
	Extras  bool     `json:"extras"`
}

func serveModulesUI(p paths, state *moduleState, addr string, open bool) error {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	mux := modulesHandler(p, state, token)
	return serveHandler(mux, addr, token, open)
}

// modulesHandler is the UI's HTTP surface: the page itself, and a JSON API
// that requires the per-run token in X-Modules-Token.
func modulesHandler(p paths, state *moduleState, token string) http.Handler {
	cur := &job{}
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Modules-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "missing or wrong token — open the URL printed by `modules ui`", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	}
	readReq := func(r *http.Request) (uiRequest, error) {
		var req uiRequest
		err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
		return req, err
	}

	mux := http.NewServeMux()
	page, _ := modulesUI.ReadFile("ui/index.html")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; font-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		w.Write(page)
	})
	uiRoot, _ := fs.Sub(modulesUI, "ui")
	fonts := http.FileServerFS(uiRoot)
	mux.HandleFunc("GET /fonts/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fonts.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/modules", authed(func(w http.ResponseWriter, r *http.Request) {
		mods, err := state.modules(r.URL.Query().Get("refresh") == "1")
		if err != nil {
			fail(w, err)
			return
		}
		dev, prod := runningStacks()
		reply(w, map[string]any{"org": state.gh.org, "modules": sortedModules(mods), "warnings": state.warnings(),
			"stacks": map[string]bool{"dev": dev, "production": prod}})
	}))
	mux.HandleFunc("POST /api/plan", authed(func(w http.ResponseWriter, r *http.Request) {
		req, err := readReq(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mods, err := state.modules(false)
		if err != nil {
			fail(w, err)
			return
		}
		pl := planModules(mods, req.Install, req.Remove, planOpts{Extras: req.Extras})
		checkRemovals(p, &pl)
		reply(w, pl)
	}))
	mux.HandleFunc("POST /api/run", authed(func(w http.ResponseWriter, r *http.Request) {
		req, err := readReq(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mode, err := parseApplyMode(req.Mode)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cur.mu.Lock()
		if cur.running {
			cur.mu.Unlock()
			http.Error(w, "a run is already in progress", http.StatusConflict)
			return
		}
		cur.running, cur.done, cur.ok = true, false, false
		cur.title = fmt.Sprintf("%d to install, %d to remove, apply: %s", len(req.Install), len(req.Remove), mode)
		cur.log.Reset()
		cur.mu.Unlock()

		go func() {
			err := func() error {
				mods, err := state.modules(false)
				if err != nil {
					return err
				}
				pl := planModules(mods, req.Install, req.Remove, planOpts{Extras: req.Extras})
				checkRemovals(p, &pl)
				g := state.git
				g.out = cur
				if err := executePlan(p, g, mods, pl); err != nil {
					return err
				}
				if pl.empty() && mode == applyNone {
					return nil
				}
				return applyPlan(p, pl, mode, cur)
			}()
			cur.mu.Lock()
			defer cur.mu.Unlock()
			if err != nil {
				fmt.Fprintf(&cur.log, "\n✗ %v\n", err)
			} else {
				cur.log.WriteString("\n✓ Done.\n")
			}
			cur.running, cur.done, cur.ok = false, true, err == nil
		}()
		reply(w, cur.snapshot())
	}))
	mux.HandleFunc("GET /api/job", authed(func(w http.ResponseWriter, r *http.Request) {
		reply(w, cur.snapshot())
	}))
	return mux
}

func serveHandler(mux http.Handler, addr, token string, open bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	} else if !ip.IsLoopback() {
		warn("listening on a non-loopback address — anyone who can reach it and has the link can install modules")
	}
	url := fmt.Sprintf("http://%s/#token=%s", net.JoinHostPort(host, port), token)
	fmt.Printf("\n%s%sNucleus modules%s  %s\n  %sCtrl-C to stop.%s\n\n", cBold, cGreen, cReset, url, cDim, cReset)
	if open {
		openBrowser(url)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
