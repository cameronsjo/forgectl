package theme

import (
	"reflect"
	"testing"
)

// TestFang_FillsEveryColorSchemeField walks fang.ColorScheme by reflection so
// a field added upstream, or one this adapter forgets, fails loudly instead
// of silently rendering the default (usually invisible) colour.
func TestFang_FillsEveryColorSchemeField(t *testing.T) {
	cs := Default().Fang()(nil)

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

func TestFang_IgnoresItsArgument(t *testing.T) {
	th := Default()
	// A nil LightDarkFunc must not be invoked — Fang() resolves from
	// th.IsDark(), never from the function fang hands it.
	got := th.Fang()(nil)
	if got.Base == nil {
		t.Fatal("Fang()(nil) panicked or returned a zero scheme")
	}
}
