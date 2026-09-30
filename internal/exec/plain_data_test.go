package exec

import (
	"context"
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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
			if err := readPlainData(&b, reflect.ValueOf(tc.v), 0); err != nil {
				t.Fatal(err)
			}
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

// maxPlainDataDepth bounds readPlainData's walk. A value nested deeper is
// an error, not a stop: bytes past the bound would go unread, and a clean
// walk would then vouch for them (#926).
const maxPlainDataDepth = 32

// readPlainData writes to b everything the plain-data readers return from v:
// every string, every byte as itself, and every other integer kind as the
// character it encodes (a secret held as []rune or []int64 reads back as
// well as one held as []byte), through pointers, interfaces, structs, slices, arrays and maps.
// It uses no reader that checks read-only (Interface) and none that hands out
// an address. It fails when v nests deeper than maxPlainDataDepth, so a
// cycle or a deep chain cannot pass unread.
func readPlainData(b *strings.Builder, v reflect.Value, depth int) error {
	if !v.IsValid() {
		return nil
	}
	if depth > maxPlainDataDepth {
		return fmt.Errorf("the plain-data walk passed depth %d at a %s; raise maxPlainDataDepth or break the cycle, since a walk that stops early vouches for bytes it never read", maxPlainDataDepth, v.Type())
	}
	switch v.Kind() {
	case reflect.String:
		b.WriteString(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(string(plainRune(v.Int())))
	case reflect.Uint8:
		b.WriteByte(byte(v.Uint())) //nolint:gosec // G115: a Uint8 kind holds a byte
	case reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		b.WriteString(string(plainRune(int64(v.Uint())))) //nolint:gosec // G115: an out-of-range value reads as U+FFFD, like any non-character
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return readPlainData(b, v.Elem(), depth+1)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if err := readPlainData(b, v.Field(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if err := readPlainData(b, v.Index(i), depth+1); err != nil {
				return err
			}
		}
		b.WriteByte(0)
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if err := readPlainData(b, it.Key(), depth+1); err != nil {
				return err
			}
			b.WriteByte(0)
			if err := readPlainData(b, it.Value(), depth+1); err != nil {
				return err
			}
			b.WriteByte(0)
		}
	}
	return nil
}

// plainRune is the character an integer encodes, U+FFFD for one that
// encodes none.
func plainRune(n int64) rune {
	if n < 0 || n > utf8.MaxRune {
		return utf8.RuneError
	}
	return rune(n) //nolint:gosec // G115: n is within [0, utf8.MaxRune]
}

// integersOf returns s's bytes as a slice of integer kind T, the shape a
// secret held as []rune or []int64 would take.
func integersOf[T ~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr](s string) []T {
	out := make([]T, len(s))
	for i := range len(s) {
		out[i] = T(s[i])
	}
	return out
}

// TestReadPlainDataReadsEveryIntegerKind pins #926: the value walk reads a
// secret held in any integer kind, not only as bytes, so a []rune or []int64
// stored in a context value or a field cannot pass it unread.
//
// Mutation that turns it red: read only reflect.Uint8 again (every row but
// []byte fails).
func TestReadPlainDataReadsEveryIntegerKind(t *testing.T) {
	const secret = "zzSECRETzzVALUEzz"
	type holder struct{ v any }
	type ctxKey struct{}
	for name, v := range map[string]any{
		"[]byte":    []byte(secret),
		"[]rune":    []rune(secret),
		"[]int":     integersOf[int](secret),
		"[]int8":    integersOf[int8](secret),
		"[]int16":   integersOf[int16](secret),
		"[]int64":   integersOf[int64](secret),
		"[]uint":    integersOf[uint](secret),
		"[]uint16":  integersOf[uint16](secret),
		"[]uint32":  integersOf[uint32](secret),
		"[]uint64":  integersOf[uint64](secret),
		"[]uintptr": integersOf[uintptr](secret),
	} {
		for how, w := range map[string]any{
			"bare":                   v,
			"in an unexported field": holder{v},
			"in a context value":     context.WithValue(context.Background(), ctxKey{}, holder{v}),
		} {
			var b strings.Builder
			if err := readPlainData(&b, reflect.ValueOf(w), 0); err != nil {
				t.Fatalf("%s %s: %v", name, how, err)
			}
			if !strings.Contains(b.String(), secret) {
				t.Errorf("%s %s: the walk did not read the secret back (%q); a secret held that way would pass it", name, how, b.String())
			}
		}
	}
}

// TestReadPlainDataFailsPastItsDepth pins #926: the value walk fails at its
// depth bound instead of stopping there, since a walk that stops early
// reports a clean read of bytes it never reached. A value nested within the
// bound still reads through.
//
// Mutation that turns it red: return nil at the bound instead of an error.
func TestReadPlainDataFailsPastItsDepth(t *testing.T) {
	const secret = "zzSECRETzz"
	nest := func(levels int) any {
		var v any = secret
		for range levels {
			v = []any{v}
		}
		return v
	}
	var b strings.Builder
	if err := readPlainData(&b, reflect.ValueOf(nest(maxPlainDataDepth/4)), 0); err != nil || !strings.Contains(b.String(), secret) {
		t.Fatalf("a value within the bound: err %v, read %q; want the secret read back", err, b.String())
	}
	type node struct{ next *node }
	cycle := &node{}
	cycle.next = cycle
	for name, v := range map[string]any{"a chain past the bound": nest(maxPlainDataDepth), "a pointer cycle": cycle} {
		if err := readPlainData(&strings.Builder{}, reflect.ValueOf(v), 0); err == nil {
			t.Errorf("%s: no error; the walk stopped silently and would vouch for what it never read", name)
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

// mentionsType reports whether t names target anywhere in its structure:
// through pointers, slices, arrays, maps, channels, function signatures,
// interface methods, the fields of an unnamed struct, and a generic
// instance's type arguments. A different named type is not entered, since
// its own declaration is checked where it is written.
func mentionsType(t types.Type, target *types.TypeName, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch t := t.(type) {
	case *types.Named:
		if t.Obj() == target || t.Origin().Obj() == target {
			return true
		}
		for a := range t.TypeArgs().Types() {
			if mentionsType(a, target, seen) {
				return true
			}
		}
	case *types.Alias:
		return mentionsType(types.Unalias(t), target, seen)
	case *types.Pointer:
		return mentionsType(t.Elem(), target, seen)
	case *types.Slice:
		return mentionsType(t.Elem(), target, seen)
	case *types.Array:
		return mentionsType(t.Elem(), target, seen)
	case *types.Chan:
		return mentionsType(t.Elem(), target, seen)
	case *types.Map:
		return mentionsType(t.Key(), target, seen) || mentionsType(t.Elem(), target, seen)
	case *types.Tuple:
		for v := range t.Variables() {
			if mentionsType(v.Type(), target, seen) {
				return true
			}
		}
	case *types.Signature:
		return mentionsType(t.Params(), target, seen) || mentionsType(t.Results(), target, seen)
	case *types.Struct:
		for f := range t.Fields() {
			if mentionsType(f.Type(), target, seen) {
				return true
			}
		}
	case *types.Interface:
		for m := range t.Methods() {
			if mentionsType(m.Type(), target, seen) {
				return true
			}
		}
		for e := range t.EmbeddedTypes() {
			if mentionsType(e, target, seen) {
				return true
			}
		}
	}
	return false
}

// storedIn returns, for every struct field declared in c's files (a named
// type's, an anonymous struct's, a function-local type's) and every
// package-level var whose type mentions target, its position and name, less
// the fields in except.
func storedIn(c *checkedPackage, target *types.TypeName, except ...*types.Var) []string {
	var out []string
	for _, f := range c.files {
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			s, ok := c.info.Types[st].Type.(*types.Struct)
			if !ok {
				return true
			}
			for fv := range s.Fields() {
				if !slices.Contains(except, fv) && mentionsType(fv.Type(), target, map[types.Type]bool{}) {
					out = append(out, c.fset.Position(fv.Pos()).String()+": field "+fv.Name()+" ("+fv.Type().String()+")")
				}
			}
			return true
		})
	}
	for _, name := range c.pkg.Scope().Names() {
		if v, ok := c.pkg.Scope().Lookup(name).(*types.Var); ok && mentionsType(v.Type(), target, map[types.Type]bool{}) {
			out = append(out, c.fset.Position(v.Pos()).String()+": package var "+v.Name()+" ("+v.Type().String()+")")
		}
	}
	return out
}

// execStruct returns internal/exec's struct type name and its field called
// field (nil for an empty name), failing the test when either is gone, since
// the rule would then check nothing.
func execStruct(t *testing.T, c *checkedPackage, p guardPlatform, name, field string) (*types.TypeName, *types.Struct, *types.Var) {
	t.Helper()
	tn, ok := c.pkg.Scope().Lookup(name).(*types.TypeName)
	if !ok {
		t.Fatalf("[%s] internal/exec declares no %s type; the rule would check nothing", p, name)
	}
	st, ok := tn.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatalf("[%s] %s is not a struct", p, name)
	}
	if field == "" {
		return tn, st, nil
	}
	for f := range st.Fields() {
		if f.Name() == field {
			return tn, st, f
		}
	}
	t.Fatalf("[%s] %s has no %s field; the rule would check nothing", p, name, field)
	return nil, nil, nil
}

// TestSealedDataHasOneHolder pins, in internal/exec's production files on
// every platform in guardPlatforms, that each closure-sealed payload has
// exactly one closure (#926):
//
//   - maskData is held only by argMask.get: no other struct field, whatever
//     its type (a second func() maskData included), and no package-level
//     var, names it. maskData's maps and slices are the plain data
//     TestMaskAndOutputHoldNoPlainData keeps out of reflect's reach; stored
//     anywhere else, they are back in it.
//   - outputBuf has exactly the fields n and read, so a second closure over
//     the captured bytes cannot join read, which
//     TestOnlyCopyBytesForParseReadsOutput pins to CopyBytesForParse.
//
// Mutations that turn it red: give CommandError a `mask maskData` field; give
// argMask a second `peek func() maskData` field; give outputBuf a
// `peek func() []byte` field.
func TestSealedDataHasOneHolder(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		md, _, _ := execStruct(t, c, p, "maskData", "")
		_, _, get := execStruct(t, c, p, "argMask", "get")
		if !mentionsType(get.Type(), md, map[types.Type]bool{}) {
			t.Fatalf("[%s] argMask.get (%s) does not mention maskData: the matcher is broken, not the package clean", p, get.Type())
		}
		for _, s := range storedIn(c, md, get) {
			t.Errorf("[%s] %s holds a maskData outside argMask.get's closure; the mask's values are plain data there (forgectl#897)", p, s)
		}
		_, ob, _ := execStruct(t, c, p, "outputBuf", "")
		var names []string
		for f := range ob.Fields() {
			names = append(names, f.Name())
		}
		if !slices.Equal(names, []string{"n", "read"}) {
			t.Errorf("[%s] outputBuf has fields %q, want exactly [n read]: a second field can hold the captured bytes past CopyBytesForParse", p, names)
		}
	}
}

// TestRawCaptureStaysInRunAndWrap pins #926's option for tailBuffer and
// ceilingWriter, which hold a child's raw, unmasked stderr tail and stdout in
// plain []byte fields: rather than sealing them, it pins that neither value
// leaves runAndWrap, which masks what it keeps before anything else sees it.
// In internal/exec's production files on every platform in guardPlatforms:
//
//   - no struct field and no package-level var has a type that mentions
//     either type, so neither is ever stored;
//   - no function or method signature mentions either, bar maskedTail's
//     parameter (it masks the tail it is handed and keeps nothing), so
//     neither is ever returned or handed on;
//   - each type is named only in its own methods, maskedTail's signature
//     and runAndWrap, so no other function makes one;
//   - each buf field is named only in its own type's methods and
//     runAndWrap.
//
// A value converted to an interface is not followed: runAndWrap hands both to
// cmd.Stderr and cmd.Stdout as io.Writers by design, and anything else it
// did with them is in the one function the last two rules keep them in.
//
// Mutations that turn it red: give CommandError a `raw *tailBuffer` field;
// declare a `func() *tailBuffer` literal in runAndWrap; build a tailBuffer in a
// helper outside runAndWrap; read a ceilingWriter's buf in maskedTail.
func TestRawCaptureStaysInRunAndWrap(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		for _, name := range []string{"tailBuffer", "ceilingWriter"} {
			tn, _, buf := execStruct(t, c, p, name, "buf")
			for _, s := range storedIn(c, tn) {
				t.Errorf("[%s] %s stores a %s; its raw capture must not outlive runAndWrap", p, s, name)
			}
			allowed := map[string]bool{"runAndWrap": true}
			if name == "tailBuffer" {
				allowed["maskedTail"] = true
			}
			namedIn, bufIn := 0, 0
			for _, f := range c.files {
				for _, decl := range f.Decls {
					fd, isFunc := decl.(*ast.FuncDecl)
					own := isFunc && fd.Recv != nil && len(fd.Recv.List) == 1 && recvBase(fd.Recv.List[0].Type) == name
					fn := ""
					if isFunc && fd.Recv == nil {
						fn = fd.Name.Name
					}
					ast.Inspect(decl, func(n ast.Node) bool {
						if ft, ok := n.(*ast.FuncType); ok {
							for _, fl := range []*ast.FieldList{ft.Params, ft.Results} {
								if fl == nil || fl == ft.Params && allowed[fn] && fn == "maskedTail" && ft == fd.Type {
									continue
								}
								for _, field := range fl.List {
									if mentionsType(c.info.Types[field.Type].Type, tn, map[types.Type]bool{}) {
										t.Errorf("[%s] %s: a signature mentions %s; its raw capture must not be returned or handed on", p, c.fset.Position(field.Pos()), name)
									}
								}
							}
						}
						id, ok := n.(*ast.Ident)
						if !ok {
							return true
						}
						switch c.info.Uses[id] {
						case tn:
							namedIn++
							if !own && !allowed[fn] {
								t.Errorf("[%s] %s: %s is named outside its own methods, maskedTail and runAndWrap", p, c.fset.Position(id.Pos()), name)
							}
						case buf:
							bufIn++
							if !own && fn != "runAndWrap" {
								t.Errorf("[%s] %s: %s.buf is named outside its own methods and runAndWrap", p, c.fset.Position(id.Pos()), name)
							}
						}
						return true
					})
				}
			}
			if namedIn == 0 || bufIn == 0 {
				t.Errorf("[%s] %s named %d times, its buf %d times: the matcher is broken, not the package clean", p, name, namedIn, bufIn)
			}
		}
	}
}

// recvBase is a receiver type expression's base type name: T for T, *T, T[K]
// and *T[K].
func recvBase(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}
