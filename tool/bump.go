package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// bump increments a module's SemVer version in place. It targets:
//   - an app       → apps/<id>/nucleus.app.json
//   - a widget     → widgets/<id>/nucleus.widget.json
//   - the platform → infra/nucleus.json   (module id "nucleus" or "platform")
//
// Only the top-level "version" string is rewritten; the rest of the manifest is
// left byte-for-byte untouched (no reserialization → no key reordering).
//
//	nucleus bump <module> <part> [channel]
//	  part:    major | minor | patch | pre | release
//	  channel: alpha | beta | rc   (optional)
//
// See infra/nucleus-docs/VERSIONING.md for the full rules.

// channelRank orders prerelease channels; they only ever advance forward.
var channelRank = map[string]int{"alpha": 1, "beta": 2, "rc": 3}

// versionField matches the first top-level `"version": "…"` in a manifest.
var versionField = regexp.MustCompile(`("version"\s*:\s*")([^"]*)(")`)

type semver struct {
	major, minor, patch int
	channel             string // "" | alpha | beta | rc
	pre                 int    // prerelease number (0 when channel == "")
}

func (v semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.channel != "" {
		s += fmt.Sprintf("-%s.%d", v.channel, v.pre)
	}
	return s
}

var semverParse = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-(alpha|beta|rc)\.(\d+))?$`)

func parseSemver(s string) (semver, error) {
	m := semverParse.FindStringSubmatch(s)
	if m == nil {
		return semver{}, fmt.Errorf("%q is not a supported SemVer version", s)
	}
	v := semver{}
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	v.channel = m[4]
	if m[5] != "" {
		v.pre, _ = strconv.Atoi(m[5])
	}
	return v, nil
}

// applyBump computes the next version. Rules:
//   - major|minor|patch          bump that component, clear any prerelease
//   - major|minor|patch <chan>   bump that component, then start -<chan>.1
//   - pre                        advance the current prerelease number
//   - pre <chan>                 promote the prerelease channel (forward only), reset to .1
//   - release                    drop the prerelease suffix (finalize)
func applyBump(v semver, part, channel string) (semver, error) {
	switch part {
	case "major":
		v = semver{major: v.major + 1}
	case "minor":
		v = semver{major: v.major, minor: v.minor + 1}
	case "patch":
		v = semver{major: v.major, minor: v.minor, patch: v.patch + 1}
	case "pre":
		if v.channel == "" {
			return v, fmt.Errorf("current version %s is stable — start a prerelease with e.g. `minor beta`, not `pre`", v)
		}
		if channel == "" || channel == v.channel {
			v.pre++
		} else {
			if channelRank[channel] < channelRank[v.channel] {
				return v, fmt.Errorf("cannot move prerelease from %s to %s — channels only advance alpha → beta → rc", v.channel, channel)
			}
			v.channel = channel
			v.pre = 1
		}
		return v, nil
	case "release":
		if v.channel == "" {
			return v, fmt.Errorf("current version %s is already stable — nothing to finalize", v)
		}
		v.channel = ""
		v.pre = 0
		return v, nil
	default:
		return v, fmt.Errorf("unknown part %q — use major|minor|patch|pre|release", part)
	}
	// major|minor|patch reach here; an optional channel starts a prerelease.
	if channel != "" {
		v.channel = channel
		v.pre = 1
	}
	return v, nil
}

// resolveModuleFile locates the manifest holding a module's version and returns
// its path plus a human label for messages.
func resolveModuleFile(p paths, module string) (string, string, error) {
	if module == "nucleus" || module == "platform" {
		f := filepath.Join(p.infra, "nucleus.json")
		if !exists(f) {
			return "", "", fmt.Errorf("platform manifest not found at %s", f)
		}
		return f, "nucleus (platform)", nil
	}
	if f := filepath.Join(p.apps, module, "nucleus.app.json"); exists(f) {
		return f, "app " + module, nil
	}
	if f := filepath.Join(p.widgets, module, "nucleus.widget.json"); exists(f) {
		return f, "widget " + module, nil
	}
	return "", "", fmt.Errorf("no app or widget %q found (looked in apps/ and widgets/; use \"nucleus\" for the platform version)", module)
}

func runBump(p paths, args []string) error {
	if len(args) < 2 {
		bumpUsage()
		return fmt.Errorf("expected: nucleus bump <module> <part> [channel]")
	}
	module, part := args[0], args[1]
	channel := ""
	if len(args) >= 3 {
		channel = args[2]
	}
	if channel != "" {
		if _, ok := channelRank[channel]; !ok {
			return fmt.Errorf("unknown channel %q — use alpha|beta|rc", channel)
		}
	}

	file, label, err := resolveModuleFile(p, module)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	loc := versionField.FindSubmatchIndex(data)
	if loc == nil {
		return fmt.Errorf("%s has no \"version\" field to bump — add one first (see VERSIONING.md)", label)
	}
	current := string(data[loc[4]:loc[5]]) // group 2 = the version string
	cur, err := parseSemver(current)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	next, err := applyBump(cur, part, channel)
	if err != nil {
		return err
	}

	// Splice the new version in, leaving everything else byte-identical.
	out := append([]byte{}, data[:loc[4]]...)
	out = append(out, next.String()...)
	out = append(out, data[loc[5]:]...)
	if err := os.WriteFile(file, out, 0o644); err != nil {
		return err
	}

	fmt.Printf("✔ %s: v%s → v%s\n", label, cur, next)
	rel, _ := filepath.Rel(p.root, file)
	fmt.Printf("  %s\n", rel)
	return nil
}

func bumpUsage() {
	fmt.Fprintln(os.Stderr, `nucleus bump — increment a module's SemVer version

Usage:
  nucleus bump <module> <part> [channel]

  <module>   app id, widget id, or "nucleus" / "platform" for the platform version
  <part>     major | minor | patch | pre | release
  [channel]  alpha | beta | rc   (optional)

Examples:
  nucleus bump echo patch            0.1.0        -> 0.1.1
  nucleus bump echo minor            0.1.3        -> 0.2.0
  nucleus bump echo minor beta       0.1.3        -> 0.2.0-beta.1
  nucleus bump echo pre              0.2.0-beta.1 -> 0.2.0-beta.2
  nucleus bump echo pre rc           0.2.0-beta.2 -> 0.2.0-rc.1   (promote channel)
  nucleus bump echo release          0.2.0-rc.1   -> 0.2.0
  nucleus bump nucleus minor         0.1.0        -> 0.2.0        (platform)`)
}
