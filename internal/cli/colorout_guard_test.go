package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestWriterBindingTracking pins the two shapes the first version of the guard
// could not see, both found in review.
//
// The guard walks a function tracking which locals hold a raw
// cmd.OutOrStdout(). Getting that wrong is not a cosmetic miss: a false
// negative here is a command shipping ANSI into pipes with a green test beside
// it, which is exactly how the three real leaks on this branch survived.
func TestWriterBindingTracking(t *testing.T) {
	// The assertion is over the state DURING the walk, not the state left at
	// the end. That distinction is the whole point of the rebinding case: `out`
	// is a raw writer between its binding and its reassignment, and a styled
	// write in that window must be flagged even though the name is safe by the
	// time the function returns.
	tests := []struct {
		name    string
		body    string
		wantRaw []string
	}{
		{
			name:    "short variable declaration",
			body:    `out := cmd.OutOrStdout()`,
			wantRaw: []string{"out"},
		},
		{
			// The var form is idiomatic Go and was ignored entirely, because the
			// first version inspected only *ast.AssignStmt.
			name:    "var declaration with initializer",
			body:    `var out = cmd.OutOrStdout()`,
			wantRaw: []string{"out"},
		},
		{
			name:    "colorOut binding is not raw",
			body:    `out := colorOut(cmd)`,
			wantRaw: nil,
		},
		{
			// Rebinding must not retroactively clear the name. A prepass that
			// computed one final set would report `out` safe here and miss any
			// styled write between the two statements.
			name: "raw then rebound to colorOut",
			body: `out := cmd.OutOrStdout()
_ = out
out = colorOut(cmd)`,
			wantRaw: []string{"out"},
		},
		{
			name:    "unrelated binding",
			body:    `out := os.Stdout`,
			wantRaw: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "package p\nfunc f(cmd *C) {\n" + tt.body + "\n}\n"
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			raw := map[string]bool{}
			everRaw := map[string]bool{}
			// Walk in source order, exactly as the guard does, recording every
			// name that is raw at any point rather than only at the end.
			ast.Inspect(file, func(n ast.Node) bool {
				noteWriterBinding(n, raw)
				for name, isRaw := range raw {
					if isRaw {
						everRaw[name] = true
					}
				}
				return true
			})
			for _, want := range tt.wantRaw {
				if !everRaw[want] {
					t.Errorf("%q: expected %q tracked as a raw writer at some point, got %v", tt.body, want, everRaw)
				}
			}
			if len(tt.wantRaw) == 0 && len(everRaw) != 0 {
				t.Errorf("%q: expected no raw writers, got %v", tt.body, everRaw)
			}
		})
	}
}

// (The compat-shim scanner is exercised against fixtures in the root package,
// where it lives — see TestFindCompatShimImports in deps_single_lipgloss_test.go.)
