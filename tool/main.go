// Command nucleus is the Nucleus infra tool. It replaces the old
// infra/generate.js (config generation) and the infra/build bash script
// (incremental frontend build + deploy) with a single compiled binary.
//
//	nucleus generate   regenerate nginx + docker-compose configs from manifests
//	nucleus build      build all frontends incrementally, then deploy the stack
//
// It is a drop-in replacement: `nucleus generate` emits byte-identical output to
// the previous generate.js, and `nucleus build` preserves the bash script's
// incremental-cache contract (with the units now built in parallel for speed).
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Paths are resolved relative to the infra/ directory, exactly like the JS
// tool resolved them from import.meta.url. The binary is run from infra/ (the
// build shim cd's there first), so we anchor on the working directory and fall
// back to NUCLEUS_INFRA when set.
type paths struct {
	infra, root, apps, widgets string
}

func resolvePaths() (paths, error) {
	infra := os.Getenv("NUCLEUS_INFRA")
	if infra == "" {
		wd, err := os.Getwd()
		if err != nil {
			return paths{}, err
		}
		infra = wd
	}
	infra, err := filepath.Abs(infra)
	if err != nil {
		return paths{}, err
	}
	root := filepath.Dir(infra)
	return paths{
		infra:   infra,
		root:    root,
		apps:    filepath.Join(root, "apps"),
		widgets: filepath.Join(root, "widgets"),
	}, nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "generate", "gen":
		if err := runGenerate(p); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "build":
		if err := runBuild(p, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "dev":
		if err := runDev(p); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "bump":
		if err := runBump(p, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `nucleus — Nucleus infra tool

Usage:
  nucleus generate          regenerate nginx + docker-compose configs from manifests
  nucleus build [--force]   incrementally build all frontends, then deploy the stack
  nucleus dev               start the dev stack (no-op if it's already running)
  nucleus bump <module> <part> [channel]
                            bump a module/platform SemVer version (see VERSIONING.md)

Flags (build):
  --force, -f     ignore the build cache and rebuild every unit
  -j N            max parallel build units (default: min(NumCPU, 4))`)
}
