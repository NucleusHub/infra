package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A build as the nucleus-web configurator (/create) downloads it: `toManifest`
// in src/services/configuration.js. A saved configuration (`installed` instead
// of `packages`) is accepted too.
type buildFile struct {
	Name       string         `json:"name"`
	Smart      bool           `json:"smart"`
	Appearance map[string]any `json:"appearance"`
	Packages   []struct {
		ID      string            `json:"id"`
		Kind    string            `json:"kind"`
		Version string            `json:"version"`
		Options map[string]string `json:"options"`
	} `json:"packages"`
	Installed []string                     `json:"installed"`
	Options   map[string]map[string]string `json:"options"`
}

type buildPart struct {
	ID, Kind string
	Options  bool
}

func (b buildFile) parts() []buildPart {
	var out []buildPart
	for _, p := range b.Packages {
		out = append(out, buildPart{ID: p.ID, Kind: p.Kind, Options: len(p.Options) > 0})
	}
	if len(b.Packages) == 0 {
		for _, id := range b.Installed {
			out = append(out, buildPart{ID: id, Options: len(b.Options[id]) > 0})
		}
	}
	return out
}

func parseBuild(data []byte) (buildFile, error) {
	var b buildFile
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("not a Nucleus build file: %w", err)
	}
	if len(b.Packages) == 0 && len(b.Installed) == 0 && b.Appearance == nil {
		return b, errors.New("not a Nucleus build file: no packages and no appearance — download it from /create on the Nucleus site")
	}
	return b, nil
}

type buildImport struct {
	Name       string      `json:"name"`
	Install    []string    `json:"install"`
	Present    []string    `json:"present"`
	Unknown    []string    `json:"unknown"`
	Notes      []string    `json:"notes"`
	Appearance *appearance `json:"appearance,omitempty"`
}

// Parts every Nucleus has; the configurator lists them, the installer doesn't manage them.
var builtinParts = map[string]bool{"core": true, "hub": true}

func resolveBuild(mods map[string]*module, b buildFile) buildImport {
	imp := buildImport{Name: strings.TrimSpace(b.Name), Install: []string{}, Present: []string{}, Unknown: []string{}, Notes: []string{}}
	seen := map[string]bool{}
	var withOptions []string
	for _, part := range b.parts() {
		if part.ID == "" || builtinParts[part.ID] {
			continue
		}
		m := findBuildPart(mods, part)
		if m == nil {
			imp.Unknown = append(imp.Unknown, part.ID)
			continue
		}
		k := m.key()
		if seen[k] {
			continue
		}
		seen[k] = true
		if part.Options {
			withOptions = append(withOptions, m.Name)
		}
		if m.Installed {
			imp.Present = append(imp.Present, k)
		} else {
			imp.Install = append(imp.Install, k)
		}
	}
	if len(imp.Unknown) > 0 {
		imp.Notes = append(imp.Notes, fmt.Sprintf("not in the marketplace catalog, skipped: %s", strings.Join(imp.Unknown, ", ")))
	}
	if len(withOptions) > 0 {
		imp.Notes = append(imp.Notes, fmt.Sprintf("package settings aren't applied by the installer yet (%s) — set them in the app", strings.Join(withOptions, ", ")))
	}
	if b.Smart {
		imp.Notes = append(imp.Notes, "the build has Nucleus Smart on — that's tied to your account, not installed from here")
	}
	if b.Appearance != nil {
		a, notes := normalizeAppearance(b.Appearance)
		a.Name = imp.Name
		imp.Appearance = &a
		imp.Notes = append(imp.Notes, notes...)
	}
	return imp
}

// The configurator names parts by their marketplace slug, which usually but not
// always matches the manifest id (widget:echo is listed as "echo-widget").
func findBuildPart(mods map[string]*module, part buildPart) *module {
	kindOK := func(m *module) bool { return part.Kind == "" || string(m.Kind) == part.Kind }
	var bySlug []*module
	for _, m := range mods {
		if m.Slug == part.ID && kindOK(m) {
			bySlug = append(bySlug, m)
		}
	}
	if len(bySlug) == 1 {
		return bySlug[0]
	}
	if part.Kind != "" {
		if m, ok := mods[part.Kind+":"+part.ID]; ok {
			return m
		}
	}
	m, _ := resolveKey(mods, part.ID)
	return m
}

// appearance is what state/appearance.json holds and core/useAppearance.js reads.
type appearance struct {
	Version      int     `json:"version"`
	Name         string  `json:"name,omitempty"`
	Theme        string  `json:"theme"`
	Accent       string  `json:"accent"`
	AccentColor  string  `json:"accentColor"`
	AccentSoft   string  `json:"accentSoft"`
	Radius       float64 `json:"radius"`
	Motion       string  `json:"motion"`
	Glass        string  `json:"glass"`
	Transparency float64 `json:"transparency"`
	TypeScale    float64 `json:"typeScale"`
	Wallpaper    string  `json:"wallpaper"`
	Stamp        string  `json:"stamp,omitempty"`
}

const appearanceVersion = 1

// Mirrors `accents` in nucleus-web src/content/configurator.js.
var accentPresets = map[string][2]string{
	"nucleus":  {"#A855F7", "#C084FC"},
	"midnight": {"#4F6BF6", "#8FA4FF"},
	"arctic":   {"#22B8CF", "#67E3F0"},
	"ember":    {"#F2711C", "#FFA463"},
	"forest":   {"#2FA36B", "#6EE7A8"},
}

var appearanceChoices = map[string][]string{
	"theme":     {"dark", "light", "system"},
	"motion":    {"full", "measured", "none"},
	"glass":     {"frosted", "clear", "solid"},
	"wallpaper": {"none", "aurora", "grid", "dust"},
}

func defaultAppearance() appearance {
	return appearance{Version: appearanceVersion, Theme: "dark", Accent: "nucleus",
		AccentColor: accentPresets["nucleus"][0], AccentSoft: accentPresets["nucleus"][1],
		Radius: 16, Motion: "full", Glass: "frosted", Transparency: 0.7, TypeScale: 1, Wallpaper: "aurora"}
}

var hexColorRe = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// Same rules as the configurator's own loader: unknown values fall back to the
// defaults rather than failing, since saves outlive the code that wrote them.
func normalizeAppearance(raw map[string]any) (appearance, []string) {
	a := defaultAppearance()
	var notes []string
	pick := func(field string, into *string) {
		v, _ := raw[field].(string)
		if v == "" {
			return
		}
		for _, c := range appearanceChoices[field] {
			if v == c {
				*into = v
				return
			}
		}
		notes = append(notes, fmt.Sprintf("appearance: unknown %s %q, using %q", field, v, *into))
	}
	num := func(field string, into *float64, lo, hi float64) {
		v, ok := raw[field].(float64)
		if !ok || math.IsNaN(v) {
			return
		}
		*into = math.Min(hi, math.Max(lo, v))
	}
	pick("theme", &a.Theme)
	pick("motion", &a.Motion)
	pick("wallpaper", &a.Wallpaper)
	if g, _ := raw["glass"].(string); g == "" {
		if frosted, ok := raw["frosted"].(bool); ok && !frosted {
			a.Glass = "clear"
		}
	} else {
		pick("glass", &a.Glass)
	}
	num("radius", &a.Radius, 0, 26)
	num("transparency", &a.Transparency, 0, 1)
	num("typeScale", &a.TypeScale, 0.9, 1.15)

	switch accent, _ := raw["accent"].(string); {
	case accent == "custom":
		c, _ := raw["customAccent"].(string)
		if hexColorRe.MatchString(c) {
			a.Accent, a.AccentColor, a.AccentSoft = "custom", strings.ToUpper(expandHex(c)), mixHex(c, "#FFFFFF", 0.35)
		} else {
			notes = append(notes, fmt.Sprintf("appearance: custom accent %q isn't a colour, using Nucleus", c))
		}
	case accentPresets[accent] != [2]string{}:
		a.Accent, a.AccentColor, a.AccentSoft = accent, accentPresets[accent][0], accentPresets[accent][1]
	case accent != "" && accent != "violet":
		notes = append(notes, fmt.Sprintf("appearance: unknown accent %q, using Nucleus", accent))
	}
	return a, notes
}

func expandHex(c string) string {
	c = strings.TrimPrefix(c, "#")
	if len(c) == 3 {
		c = string([]byte{c[0], c[0], c[1], c[1], c[2], c[2]})
	}
	return "#" + c
}

func mixHex(a, b string, t float64) string {
	pa, _ := strconv.ParseUint(strings.TrimPrefix(expandHex(a), "#"), 16, 32)
	pb, _ := strconv.ParseUint(strings.TrimPrefix(expandHex(b), "#"), 16, 32)
	ch := func(v uint64, shift uint) float64 { return float64((v >> shift) & 255) }
	out := "#"
	for _, s := range []uint{16, 8, 0} {
		out += fmt.Sprintf("%02X", int(math.Round(ch(pa, s)*(1-t)+ch(pb, s)*t)))
	}
	return out
}

func (a appearance) summary() string {
	accent := a.Accent
	if accent == "custom" {
		accent = a.AccentColor
	}
	return fmt.Sprintf("%s theme, %s accent, %s glass, %s wallpaper, radius %g, type ×%g, motion %s",
		a.Theme, accent, a.Glass, a.Wallpaper, a.Radius, a.TypeScale, a.Motion)
}

func appearancePath(p paths) string { return filepath.Join(p.root, "state", "appearance.json") }

// Every write gets a new stamp, which is how core tells a fresh import (whose
// theme wins over an earlier choice) from one it has already applied.
func writeAppearance(p paths, a appearance) error {
	a.Stamp = time.Now().UTC().Format(time.RFC3339Nano)
	path := appearancePath(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readAppearance(p paths) (*appearance, error) {
	b, err := os.ReadFile(appearancePath(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a appearance
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", appearancePath(p), err)
	}
	return &a, nil
}

func printImport(imp buildImport) {
	title := imp.Name
	if title == "" {
		title = "build"
	}
	fmt.Printf("\n%sImporting %s%s\n", cBold, title, cReset)
	if len(imp.Present) > 0 {
		sort.Strings(imp.Present)
		fmt.Printf("  %s· already installed: %s%s\n", cDim, strings.Join(imp.Present, ", "), cReset)
	}
	if imp.Appearance != nil {
		fmt.Printf("  %s~ appearance%s %s\n", cGreen, cReset, imp.Appearance.summary())
	}
	for _, n := range imp.Notes {
		fmt.Printf("  %s⚠ %s%s\n", cYellow, n, cReset)
	}
}
