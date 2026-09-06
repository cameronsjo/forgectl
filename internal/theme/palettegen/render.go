// Package palettegen generates internal/theme/artificer_gen.go from the
// vendored Artificer palette at internal/theme/artificer/_palette.json. It is
// importable (both cmd/main.go and internal/theme's freshness test use it)
// rather than a script, so the generator and the test that checks its output
// is current can never drift apart on how rendering works — only on whether
// it was re-run.
package palettegen

import (
	"encoding/json"
	"fmt"
	"go/format"
	"regexp"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// roleGoNames is each role's exported Go identifier, which a generated array
// literal needs — palettegen cannot reach theme's unexported lowercase name
// list, and "RoleOK" is not derivable from "ok" anyway.
//
// It is keyed BY ROLE rather than being a slice parallel to
// theme.ArtificerRoleSources. A parallel slice pairs by position, so
// reordering or inserting a role in one list and not the other still passes a
// length check while silently shifting every colour from the divergence point
// onward — no compile error, no generation error, just wrong colours. Keying
// by the role itself makes that mistake unrepresentable.
var roleGoNames = map[theme.Role]string{
	theme.RoleAccent:        "RoleAccent",
	theme.RoleOK:            "RoleOK",
	theme.RoleDanger:        "RoleDanger",
	theme.RoleWarn:          "RoleWarn",
	theme.RoleActive:        "RoleActive",
	theme.RoleMuted:         "RoleMuted",
	theme.RoleMeta:          "RoleMeta",
	theme.RoleDim:           "RoleDim",
	theme.RoleFg:            "RoleFg",
	theme.RoleSteel:         "RoleSteel",
	theme.RoleBrand:         "RoleBrand",
	theme.RoleSurfaceRaised: "RoleSurfaceRaised",
	theme.RoleAccentFill:    "RoleAccentFill",
	theme.RoleOnAccent:      "RoleOnAccent",
	theme.RoleUrgentFill:    "RoleUrgentFill",
	theme.RoleOnUrgent:      "RoleOnUrgent",
	theme.RoleBg:            "RoleBg",
}

// versionRe is what a $version may contain. Deliberately an ALLOWLIST: this
// value reaches generated Go source as bare text, and enumerating the
// characters that would be dangerous there is a list that is never finished.
var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,31}$`)

// hexRe is the same idea for a colour. Every hex is written through %q, so a
// quote could not escape the literal — but a value that is not a colour has no
// business being generated into the palette either, and Theme.Hex hands its
// result straight to a terminal.
var hexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// checkHex rejects a palette value that is not a #rrggbb colour.
func checkHex(v string) error {
	if !hexRe.MatchString(v) {
		return fmt.Errorf("%q is not a #rrggbb colour", v)
	}
	return nil
}

// paletteFile is the shape of _palette.json this generator reads. Only the
// fields Render needs are declared — the file carries $description, $roles,
// and $notes too, which are documentation for a human editing the palette by
// hand and irrelevant to code generation.
type paletteFile struct {
	Version string            `json:"$version"`
	Dark    map[string]string `json:"dark"`
	Light   map[string]string `json:"light"`
}

// Render parses palette (the bytes of _palette.json) and emits Go source
// declaring `func Artificer() Palette`, with every role's dark and light hex
// resolved through theme.ArtificerRoleSources. The result is run through
// go/format before being returned, so a generated file never needs a
// separate gofmt pass.
//
// Render errors, rather than emitting an empty hex, when palette is missing a
// key any RoleSource needs — a silently-blank role would compile and then
// render invisible text.
func Render(palette []byte) ([]byte, error) {
	// Every role must have a Go identifier. A length check would not be
	// enough even when the two agreed on count: this asserts that each role
	// present in the source list is one this generator can actually name.
	for _, src := range theme.ArtificerRoleSources {
		if _, ok := roleGoNames[src.Role]; !ok {
			return nil, fmt.Errorf("palettegen: role %d (palette key %q) has no Go identifier in roleGoNames; add it",
				src.Role, src.PaletteKey)
		}
	}
	if len(roleGoNames) != len(theme.ArtificerRoleSources) {
		return nil, fmt.Errorf("palettegen: roleGoNames has %d entries, ArtificerRoleSources has %d; a role is named but not sourced, or sourced twice",
			len(roleGoNames), len(theme.ArtificerRoleSources))
	}

	var pf paletteFile
	if err := json.Unmarshal(palette, &pf); err != nil {
		return nil, fmt.Errorf("palettegen: parse palette JSON: %w", err)
	}
	if pf.Version == "" {
		return nil, fmt.Errorf("palettegen: palette JSON has no $version")
	}
	// $version is the only palette value interpolated as bare text rather than
	// through %q, so it is the one code-injection surface in this generator.
	//
	// Measured, not assumed: with this check removed, three of the five hostile
	// versions in TestRender_RejectsCodeInjectionViaVersion are still rejected,
	// because the version lands TWICE and the first site is above
	// `package theme` — an injected import or func there makes the file invalid
	// and go/format refuses. So the newline injection is not exploitable as it
	// first appears. Two shapes DID get through, and the general point stands:
	// this value reaches generated Go as text, format.Source is the only thing
	// between it and a committed "DO NOT EDIT" file, and the freshness test
	// cannot help because it re-runs this same function and compares its own
	// output. An allowlist is the cheap way to stop reasoning about it.
	if !versionRe.MatchString(pf.Version) {
		return nil, fmt.Errorf("palettegen: $version %q is not a plain version string (want %s); "+
			"it is interpolated into generated Go source and must not carry newlines or punctuation",
			pf.Version, versionRe)
	}

	var entries []string
	var missing []string
	for _, src := range theme.ArtificerRoleSources {
		goName := roleGoNames[src.Role]
		dark, darkOK := pf.Dark[src.PaletteKey]
		light, lightOK := pf.Light[src.PaletteKey]
		if !darkOK || dark == "" {
			missing = append(missing, fmt.Sprintf("dark.%s (role %s)", src.PaletteKey, goName))
		}
		if !lightOK || light == "" {
			missing = append(missing, fmt.Sprintf("light.%s (role %s)", src.PaletteKey, goName))
		}
		if darkOK && lightOK && dark != "" && light != "" {
			if err := checkHex(dark); err != nil {
				return nil, fmt.Errorf("palettegen: dark.%s: %w", src.PaletteKey, err)
			}
			if err := checkHex(light); err != nil {
				return nil, fmt.Errorf("palettegen: light.%s: %w", src.PaletteKey, err)
			}
			entries = append(entries, fmt.Sprintf("\t\t\t%s: {Dark: %q, Light: %q},", goName, dark, light))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("palettegen: palette JSON is missing required keys: %s", strings.Join(missing, ", "))
	}

	src := fmt.Sprintf(`// Code generated by palettegen from %s (version %s). DO NOT EDIT.

package theme

// Artificer returns the Artificer role palette, resolved from the vendored
// palette at internal/theme/%s (version %s).
func Artificer() Palette {
	return Palette{
		Name: "Artificer",
		Roles: [numRoles]Pair{
%s
		},
	}
}
`, theme.PalettePath, pf.Version, theme.PalettePath, pf.Version, strings.Join(entries, "\n"))

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return nil, fmt.Errorf("palettegen: gofmt generated source: %w", err)
	}
	return formatted, nil
}
