package theme

import (
	"fmt"
	"reflect"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestFang_FillsEveryColorSchemeField walks fang.ColorScheme by reflection so
// a field added upstream, or one this adapter forgets, fails loudly instead
// of silently rendering the default (usually invisible) colour.
func TestFang_FillsEveryColorSchemeField(t *testing.T) {
	cs := Default().Fang(true)(nil)

	v := reflect.ValueOf(cs)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		val := v.Field(i)
		switch val.Kind() {
		case reflect.Array: // ErrorHeader [2]color.Color
			for j := 0; j < val.Len(); j++ {
				if val.Index(j).IsNil() {
					t.Errorf("fang.ColorScheme.%s[%d] is nil", field.Name, j)
				}
			}
		case reflect.Interface, reflect.Pointer:
			if val.IsNil() {
				t.Errorf("fang.ColorScheme.%s is zero/nil", field.Name)
			}
		default:
			t.Errorf("fang.ColorScheme.%s has unexpected kind %v", field.Name, val.Kind())
		}
	}
}

// TestFang_ResolvesBackground pins the whole decision table: fang's answer is
// used iff the mode is auto, the caller trusts the probe, and fang supplied a
// function; every other cell resolves from t.IsDark().
func TestFang_ResolvesBackground(t *testing.T) {
	modes := []struct {
		name string
		mode Mode
	}{{"auto", ModeAuto}, {"dark", ModeDark}, {"light", ModeLight}}
	lds := []struct {
		name string
		ld   lipgloss.LightDarkFunc
		dark bool
	}{
		{"fangDark", lipgloss.LightDark(true), true},
		{"fangLight", lipgloss.LightDark(false), false},
		{"nil", nil, false},
	}
	for _, m := range modes {
		for _, trust := range []bool{true, false} {
			for _, l := range lds {
				for _, themeDark := range []bool{true, false} {
					name := fmt.Sprintf("%s/trust=%v/%s/themeDark=%v", m.name, trust, l.name, themeDark)
					t.Run(name, func(t *testing.T) {
						th := New(Options{Mode: m.mode}, themeDark)
						want := th.IsDark()
						if m.mode == ModeAuto && trust && l.ld != nil {
							want = l.dark
						}
						ref := th.WithDark(want)
						// Vacuity guard: if light and dark rendered the same,
						// every assertion below would pass for any answer.
						if th.WithDark(true).Color(RoleFg) == th.WithDark(false).Color(RoleFg) ||
							th.WithDark(true).Color(RoleAccent) == th.WithDark(false).Color(RoleAccent) {
							t.Fatal("light and dark palettes are indistinguishable; test would be vacuous")
						}
						cs := th.Fang(trust)(l.ld)
						if cs.Base != ref.Color(RoleFg) {
							t.Errorf("Base = %v, want %v (isDark=%v)", cs.Base, ref.Color(RoleFg), want)
						}
						if cs.Title != ref.Color(RoleAccent) {
							t.Errorf("Title = %v, want %v (isDark=%v)", cs.Title, ref.Color(RoleAccent), want)
						}
					})
				}
			}
		}
	}
}
