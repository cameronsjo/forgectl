package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// paletteOwner is the one package allowed to name a colour. It IS the palette;
// that is the point of it.
var paletteOwner = filepath.Join("internal", "theme")

// escapeExemptDirs may write a raw escape sequence, and nothing more.
//
// internal/termsafe reasons ABOUT escapes — neutralising them is its whole
// job — so it cannot do that without writing one. It gets no exemption from
// the colour checks: reasoning about escapes is not a reason to name a hex,
// and skipping the directory outright would have granted both.
var escapeExemptDirs = []string{
	filepath.Join("internal", "termsafe"),
}

// lipglossPath is the import path a colour constructor has to come from.
// Aliases are resolved per file from the import block rather than listed:
// a fixed list is a guess about how the next person will spell the import,
// and `palette "charm.land/lipgloss/v2"` walks straight past a guess while
// reading exactly like the thing the guard forbids.
const lipglossPath = "charm.land/lipgloss/v2"

// huhPath is the same, for the huh theme constructors.
const huhPath = "charm.land/huh/v2"

// importAliases returns the identifiers a file refers to importPath by —
// the explicit alias when there is one, otherwise the package's own name.
// A dot-import is reported as "." so the caller can decide; forgectl has
// none, and a blank import binds no identifier at all.
func importAliases(file *ast.File, importPath string) map[string]bool {
	out := map[string]bool{}
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name != "_" {
				out[spec.Name.Name] = true
			}
			continue
		}
		// No alias: the identifier is the last path element, which holds for
		// every charm.land package (the /v2 suffix is a major-version
		// element, not the package name).
		parts := strings.Split(p, "/")
		name := parts[len(parts)-1]
		if strings.HasPrefix(name, "v") && len(parts) > 1 {
			name = parts[len(parts)-2]
		}
		out[name] = true
	}
	return out
}

// TestNoColorLiteralsOutsideTheme is the guard that makes the migration stick.
//
// Before it, colour lived in three places — internal/tui's own palette,
// internal/cli's 256-colour marks, and internal/k8s's hand-written escape
// sequences — and each drifted from the design system independently. A rule
// that is only written down gets violated by the next person who needs a
// colour and does not know where the palette lives; this is the version a
// compiler enforces.
//
// Forbidden outside internal/theme: a lipgloss.Color call, a huh theme
// constructor, an import of the lipgloss v1 compat shim, and — in production
// code only — a string literal carrying an ESC.
//
// The lipgloss and huh checks apply to test files too; the escape check does
// not, for a reason documented at the exemption below. It found a real
// leftover on its first run: keymap.DarkCharm, the dark pin added when the
// charm v2 migration exposed huh defaulting to light, still standing beside
// the Theme.Huh() that replaced it.
func TestNoColorLiteralsOutsideTheme(t *testing.T) {
	fset := token.NewFileSet()
	scanned := 0

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", "dist":
				return filepath.SkipDir
			}
			// Only the palette owner is skipped wholesale. Everything else is
			// walked, so the colour checks still apply inside a directory that
			// is merely allowed to write an escape.
			if path == paletteOwner {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// This file necessarily names the things it forbids.
		if filepath.Base(path) == "theme_literals_test.go" {
			return nil
		}

		isTest := strings.HasSuffix(path, "_test.go")
		escapeExempt := false
		for _, dir := range escapeExemptDirs {
			if strings.HasPrefix(path, dir+string(filepath.Separator)) {
				escapeExempt = true
			}
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Errorf("%s: %v", path, parseErr)
			return nil
		}
		scanned++
		for _, finding := range inspectFile(fset, file, path, isTest, escapeExempt) {
			t.Error(finding)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Go files; the guard matched almost nothing and could not have failed", scanned)
	}
}

// inspectFile is the guard's whole judgement, extracted so it can be run
// against source of the test's own choosing.
//
// It is separate for one reason: the matchers are code, and a widened matcher
// that still reports zero findings over a clean tree is indistinguishable from
// one that stopped matching. TestGuardMatchers below feeds it violations it
// must catch and legitimate code it must not.
func inspectFile(fset *token.FileSet, file *ast.File, path string, isTest, escapeExempt bool) []string {
	var findings []string
	at := func(pos token.Pos) string {
		return fmt.Sprintf("%s:%d", path, fset.Position(pos).Line)
	}
	lipglossNames := importAliases(file, lipglossPath)
	huhNames := importAliases(file, huhPath)

	ast.Inspect(file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return true
			}
			// Test files are exempt from the escape check, and the reason
			// is worth stating because the first version of this guard got
			// it wrong.
			//
			// The intent was to forbid a test hardcoding a palette colour,
			// since that couples an unrelated test to a hex. But an
			// injected escape looks EXACTLY like a palette one — the nine
			// files that tripped the first attempt were all hostile
			// fixtures: a PR title carrying \x1b[31m, a fake trusted-marker
			// renderer. "Palette pin" and "attack payload" are the same
			// bytes, so no pattern separates them, and a rule that cannot
			// tell them apart would block the security tests to protect a
			// convention. Production code is where the rule has teeth.
			if isTest || escapeExempt {
				return true
			}
			// Decode the literal rather than pattern-matching its source
			// text. Go spells ESC at least five ways — `\x1b`, `\033`,
			// as \u001b, as \U0000001b, and as the raw byte — so a check
			// against a list of spellings is a check against the ones
			// its author thought of. Unquoting collapses all of them to
			// one byte, and a raw string literal (which cannot contain an
			// escape sequence at all) still yields its literal contents.
			decoded, unquoteErr := strconv.Unquote(e.Value)
			if unquoteErr != nil {
				// An unparseable literal in a file that parsed is not a
				// thing that happens; say so rather than skipping, since
				// a silent skip here is a hole in the guard.
				findings = append(findings, fmt.Sprintf("%s: could not unquote string literal: %v", at(e.Pos()), unquoteErr))
				return true
			}
			if strings.ContainsRune(decoded, '\x1b') {
				findings = append(findings, at(e.Pos())+": string literal carries an ANSI escape; render through internal/theme instead")
			}
		case *ast.SelectorExpr:
			pkg, ok := e.X.(*ast.Ident)
			if !ok {
				return true
			}
			// The whole Color family, not just Color. RGBColor, ANSIColor,
			// Color256, AdaptiveColor and CompleteColor all name a colour
			// just as directly, and a guard that lists one of them invites
			// the next person to reach for a sibling.
			if lipglossNames[pkg.Name] && strings.Contains(e.Sel.Name, "Color") {
				findings = append(findings, fmt.Sprintf("%s: %s.%s outside internal/theme; add a role to the palette instead", at(e.Pos()), pkg.Name, e.Sel.Name))
			}
			if huhNames[pkg.Name] && strings.HasPrefix(e.Sel.Name, "Theme") {
				findings = append(findings, fmt.Sprintf("%s: %s.%s outside internal/theme; use Theme.Huh()", at(e.Pos()), pkg.Name, e.Sel.Name))
			}
		case *ast.ImportSpec:
			if p, unquoteErr := strconv.Unquote(e.Path.Value); unquoteErr == nil &&
				strings.HasSuffix(p, "lipgloss/v2/compat") {
				findings = append(findings, path+": imports the lipgloss v1 compatibility shim")
			}
		}
		return true
	})
	return findings
}

// TestGuardMatchers runs the guard against source written to break it.
//
// Two of these cases exist because an automated reviewer found them after the
// guard shipped, and both had the same shape: the matcher recognised the one
// spelling its author had in front of them. The alias case
// (palette "charm.land/lipgloss/v2" then palette.Color) walked past a
// hardcoded name list, and the unicode-escape case walked past a source-text
// search for the two spellings that had been thought of. Neither could have
// been caught by running the guard over a clean tree, which is what "the
// guard passes" had meant until now.
func TestGuardMatchers(t *testing.T) {
	const header = "package p\n\n" +
		"import (\n" +
		"\tpalette \"charm.land/lipgloss/v2\"\n" +
		"\tlipgloss \"charm.land/lipgloss/v2\"\n" +
		"\tforms \"charm.land/huh/v2\"\n" +
		"\tnotcolor \"example.com/notlipgloss\"\n" +
		")\n\n" +
		"var _ = notcolor.Nothing\n"

	cases := []struct {
		name string
		src  string
		want bool // want at least one finding
	}{
		{"aliased lipgloss colour", `var _ = palette.Color("#ff0000")`, true},
		{"plain lipgloss colour", `var _ = lipgloss.Color("#ff0000")`, true},
		{"lipgloss colour sibling", `var _ = palette.AdaptiveColor{}`, true},
		{"aliased huh theme", `var _ = forms.ThemeCharm()`, true},
		{"escape as hex", `var _ = "\x1b[31m"`, true},
		{"escape as short unicode", `var _ = "\u001b[31m"`, true},
		{"escape as long unicode", `var _ = "\U0000001b[31m"`, true},
		{"escape as octal", `var _ = "\033[31m"`, true},
		// The other side. A matcher that flags these is too wide, and a guard
		// nobody can satisfy gets deleted rather than obeyed.
		{"same selector on an unrelated package", `var _ = notcolor.Color("#ff0000")`, false},
		{"a word containing colour, not a constructor", `var _ = "the Colorado river"`, false},
		{"a hex that is not a colour call", `var _ = "#ff0000"`, false},
		{"a huh selector that is not a theme", `var _ = forms.NewForm()`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", header+tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got := inspectFile(fset, file, "fixture.go", false, false)
			if (len(got) > 0) != tc.want {
				t.Errorf("findings = %v, want any = %v", got, tc.want)
			}
		})
	}
}

// TestGuardExemptions pins that each exemption narrows what it claims to and
// nothing else. A SkipDir-shaped exemption granting more than intended is the
// defect this branch already fixed once, for internal/termsafe.
func TestGuardExemptions(t *testing.T) {
	const src = "package p\n\n" +
		"import lipgloss \"charm.land/lipgloss/v2\"\n\n" +
		"var _ = lipgloss.Color(\"#ff0000\")\n" +
		"var _ = \"\\x1b[31m\"\n"

	for name, tc := range map[string]struct {
		isTest, escapeExempt bool
		wantEscape           bool
	}{
		"production code": {false, false, true},
		"test file":       {true, false, false},
		"escape exempt":   {false, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got := inspectFile(fset, file, "fixture.go", tc.isTest, tc.escapeExempt)

			var sawEscape, sawColor bool
			for _, f := range got {
				if strings.Contains(f, "ANSI escape") {
					sawEscape = true
				}
				if strings.Contains(f, "add a role to the palette") {
					sawColor = true
				}
			}
			if sawEscape != tc.wantEscape {
				t.Errorf("escape finding = %v, want %v (%v)", sawEscape, tc.wantEscape, got)
			}
			// The colour check applies everywhere. Neither exemption reaches
			// it — keeping them separate is the whole point.
			if !sawColor {
				t.Errorf("colour finding missing under %s; the exemption widened past escapes: %v", name, got)
			}
		})
	}
}
