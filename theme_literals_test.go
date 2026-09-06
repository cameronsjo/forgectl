package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// themeExemptDirs are the paths allowed to name a colour or an escape.
//
//   - internal/theme owns the palette; that is the point of the package.
//   - internal/termsafe and its test helper reason ABOUT escape sequences, so
//     they must be able to write one.
var themeExemptDirs = []string{
	filepath.Join("internal", "theme"),
	filepath.Join("internal", "termsafe"),
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
			for _, exempt := range themeExemptDirs {
				if path == exempt {
					return filepath.SkipDir
				}
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
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Errorf("%s: %v", path, parseErr)
			return nil
		}
		scanned++

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
				if isTest {
					return true
				}
				// The literal's raw text, so an escape written as \x1b is
				// caught as well as one typed literally.
				if strings.Contains(e.Value, `\x1b`) || strings.Contains(e.Value, `\033`) || strings.Contains(e.Value, "\x1b") {
					t.Errorf("%s:%d: string literal carries an ANSI escape; render through internal/theme instead",
						path, fset.Position(e.Pos()).Line)
				}
			case *ast.SelectorExpr:
				pkg, ok := e.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkg.Name == "lipgloss" && e.Sel.Name == "Color" {
					t.Errorf("%s:%d: lipgloss.Color outside internal/theme; add a role to the palette instead",
						path, fset.Position(e.Pos()).Line)
				}
				if pkg.Name == "huh" && strings.HasPrefix(e.Sel.Name, "Theme") {
					t.Errorf("%s:%d: huh.%s outside internal/theme; use Theme.Huh()",
						path, fset.Position(e.Pos()).Line, e.Sel.Name)
				}
			case *ast.ImportSpec:
				if p, unquoteErr := strconv.Unquote(e.Path.Value); unquoteErr == nil &&
					strings.HasSuffix(p, "lipgloss/v2/compat") {
					t.Errorf("%s: imports the lipgloss v1 compatibility shim", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Go files; the guard matched almost nothing and could not have failed", scanned)
	}
}
