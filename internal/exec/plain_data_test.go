package exec

import (
	"context"
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"strings"
	"testing"
)

// TestMaskAndOutputHoldNoPlainData pins forgectl#897's option (a): an
// argMask (which a context's value chain carries to any caller) and a
// BoundedOutput hold their secret only behind a closure, never in a field
// reflect's plain-data readers reach. Value.String, Bytes, Index, Uint,
// MapKeys and the rest skip the read-only check that guards Interface, so an
// unexported string, slice or map field is readable by any code holding the
// value, without Addr, UnsafePointer or an "unsafe" import, all of which
// TestNoFileReadsMemoryThroughReflect refuses. A func value's captures are no
// field those readers can walk into.
//
// It checks two things per type. The type walk refuses any field, at any
// depth through pointers and containers, of a kind that holds data a plain
// reader returns: a string, a slice, an array, a map, a channel, an interface
// or an unsafe.Pointer. The value walk reads a live value with only those
// readers, through every pointer, interface, container and struct field, and
// refuses one whose readable bytes contain the secret; it reads a context
// too, since the context is how an argMask reaches a caller.
//
// Mutations that turn it red: give argMask a `values []string` field again
// (fill it in WithMaskedAssignments), or give outputBuf a `data []byte`
// field again (fill it in newOutputBuf).
func TestMaskAndOutputHoldNoPlainData(t *testing.T) {
	const secret = "zzSECRETzzVALUEzz"
	const short = "sh0rt"
	ctx := WithMaskedAssignments(context.Background(), []string{"TOKEN=" + secret, "S=" + short})
	mask := maskFrom(ctx).withValues([]string{secret + "-extra"})
	if got := mask.text("x " + secret + " y"); strings.Contains(got, secret) {
		t.Fatalf("the mask does not scrub its own secret (%q); the fixture is broken, not the type clean", got)
	}
	out := BoundedOutputForTest([]byte(secret), OutputComplete)
	if data, _ := out.CopyBytesForParse(); string(data) != secret {
		t.Fatalf("CopyBytesForParse = %q, want the secret; the fixture is broken, not the type clean", data)
	}
	for _, tc := range []struct {
		name string
		v    any
		typ  bool // walk the type as well as the value
	}{
		{"an argMask", mask, true},
		{"a BoundedOutput", out, true},
		{"a SensitiveResult", SensitiveResult{Stdout: out, Stderr: out}, true},
		{"the masking context", ctx, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.typ {
				for _, path := range plainDataFields(reflect.TypeOf(tc.v), reflect.TypeOf(tc.v).String(), map[reflect.Type]bool{}) {
					t.Errorf("%s holds plain data that reflect's String, Bytes, Index or MapKeys read without the read-only check; hold it behind a closure, as sealed.Value does (forgectl#897)", path)
				}
			}
			var b strings.Builder
			readPlainData(&b, reflect.ValueOf(tc.v), 0)
			for _, s := range []string{secret, short} {
				if strings.Contains(b.String(), s) {
					t.Errorf("reflect's plain-data readers reach %q in %s; hold it behind a closure, as sealed.Value does (forgectl#897)", s, tc.name)
				}
			}
		})
	}
}

// plainDataFields returns the path of every field reachable from t, through
// pointers, structs and containers, whose kind holds data a plain reflect
// reader returns. A func is not entered: its captures are not fields.
func plainDataFields(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	switch t.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map, reflect.Chan, reflect.Interface, reflect.UnsafePointer, reflect.Uintptr:
		return []string{path + " (" + t.String() + ")"}
	case reflect.Pointer:
		return plainDataFields(t.Elem(), "(*"+path+")", seen)
	case reflect.Struct:
		if seen[t] {
			return nil
		}
		seen[t] = true
		defer delete(seen, t)
		var out []string
		for f := range t.Fields() {
			out = append(out, plainDataFields(f.Type, path+"."+f.Name, seen)...)
		}
		return out
	}
	return nil
}

// readPlainData writes to b everything the plain-data readers return from v:
// every string, and every integer as the character it encodes, through
// pointers, interfaces, structs, slices, arrays and maps. It uses no reader
// that checks read-only (Interface) and none that hands out an address.
func readPlainData(b *strings.Builder, v reflect.Value, depth int) {
	if !v.IsValid() || depth > 32 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		b.WriteString(v.String())
	case reflect.Uint8:
		_, _ = fmt.Fprintf(b, "%c", v.Uint())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			readPlainData(b, v.Elem(), depth+1)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			readPlainData(b, v.Field(i), depth+1)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			readPlainData(b, v.Index(i), depth+1)
		}
		b.WriteByte(0)
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			readPlainData(b, it.Key(), depth+1)
			b.WriteByte(0)
			readPlainData(b, it.Value(), depth+1)
			b.WriteByte(0)
		}
	}
}

// TestOnlyCopyBytesForParseReadsOutput pins, in internal/exec's production
// files on every platform in guardPlatforms, that outputBuf's read closure is
// named only inside BoundedOutput.CopyBytesForParse, apart from the key that
// sets it in newOutputBuf's literal. CopyBytesForParse is the one way out for
// captured bytes: it hands back a copy together with the completeness flag,
// so a caller cannot parse a cut-off stream without being told.
//
// Mutation that turns it red: have Len return len(b.buf.read()).
func TestOnlyCopyBytesForParseReadsOutput(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		tn, ok := c.pkg.Scope().Lookup("outputBuf").(*types.TypeName)
		if !ok {
			t.Fatalf("[%s] internal/exec declares no outputBuf type; the rule would check nothing", p)
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("[%s] outputBuf is not a struct", p)
		}
		var read *types.Var
		for f := range st.Fields() {
			if f.Name() == "read" {
				read = f
			}
		}
		if read == nil {
			t.Fatalf("[%s] outputBuf has no read field; the rule would check nothing", p)
		}
		inside := 0
		for _, f := range c.files {
			for _, decl := range f.Decls {
				fd, isFunc := decl.(*ast.FuncDecl)
				door := isFunc && fd.Name.Name == "CopyBytesForParse" && fd.Recv != nil &&
					len(fd.Recv.List) == 1 && types.ExprString(fd.Recv.List[0].Type) == "BoundedOutput"
				ctor := isFunc && fd.Name.Name == "newOutputBuf" && fd.Recv == nil
				keys := map[*ast.Ident]bool{}
				ast.Inspect(decl, func(n ast.Node) bool {
					if kv, ok := n.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							keys[id] = true
						}
					}
					id, ok := n.(*ast.Ident)
					if !ok || c.info.Uses[id] != read {
						return true
					}
					switch {
					case door:
						inside++
					case ctor && keys[id]:
					default:
						t.Errorf("[%s] %s: outputBuf.read is named outside BoundedOutput.CopyBytesForParse; captured bytes leave only through it, with the completeness flag",
							p, c.fset.Position(id.Pos()))
					}
					return true
				})
			}
		}
		if inside == 0 {
			t.Errorf("[%s] no use of outputBuf.read inside CopyBytesForParse at all: the matcher is broken, not the package clean", p)
		}
	}
}
