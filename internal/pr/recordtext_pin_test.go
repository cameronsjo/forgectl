package pr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordTextFields are the record and report fields that carry free text:
// every value written to one is escaped and capped at the source (#934,
// #963). The breadcrumb's own fields need breadcrumbText, whose byte cap is
// what keeps the record inside maxBreadcrumbRecordBytes; the report fields
// take either capper.
var recordTextFields = map[string][]string{
	"Error":        {"recordText", "breadcrumbText"},
	"Refusal":      {"recordText", "breadcrumbText"},
	"LastError":    {"breadcrumbText"},
	"RepairReason": {"breadcrumbText"},
}

// recordTextIdents are the identifiers a pinned field may be assigned
// directly, each with where its value was already capped.
var recordTextIdents = map[string]string{
	"lastError":            "settleDrainFailure sets it from breadcrumbText and hands it to recordParkedAttempt",
	"errLocalNotDrainable": "a string constant",
}

// TestRecordTextFieldsAreCappedAtTheSource is #963 B's pin: every assignment
// to Error, Refusal, LastError or RepairReason in the package, as a
// statement or a composite-literal key, is a string literal, an allowlisted
// identifier, or a call to the capper that field takes. The sites #963 named
// (the drain claim refusal and pr repair's four item.Error writes) reached
// --json raw because nothing checked this.
//
// Mutations that turn it red: set report.Refusal = err.Error() in
// claimQueued (drain.go); set item.Error = safeErrString(err) in any of
// repair.go's four failure arms; set the exhausted-attempts RepairReason in
// settleDrainFailure with a bare fmt.Sprintf; write bc.RepairReason with
// recordText in markNeedsRepairLocked (the rune cap without the byte cap).
func TestRecordTextFieldsAreCappedAtTheSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		check := func(field string, value ast.Expr) {
			cappers, ok := recordTextFields[field]
			if !ok {
				return
			}
			checked++
			if !cappedValue(value, cappers) {
				t.Errorf("%s: %s is assigned without %s; escape and cap it at the source",
					fset.Position(value.Pos()), field, strings.Join(cappers, " or "))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) != len(n.Rhs) {
					return true
				}
				for i, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						check(sel.Sel.Name, n.Rhs[i])
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok {
					check(key.Name, n.Value)
				}
			}
			return true
		})
	}
	// drain.go alone holds a Refusal, three LastError and three RepairReason
	// writes; far fewer means the walk is broken, not the package clean.
	if checked < 20 {
		t.Fatalf("checked %d assignments; the walk found too few to mean anything", checked)
	}
}

func cappedValue(value ast.Expr, cappers []string) bool {
	switch v := ast.Unparen(value).(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.Ident:
		_, ok := recordTextIdents[v.Name]
		return ok
	case *ast.CallExpr:
		fn, ok := v.Fun.(*ast.Ident)
		if !ok {
			return false
		}
		for _, c := range cappers {
			if fn.Name == c {
				return true
			}
		}
	}
	return false
}
