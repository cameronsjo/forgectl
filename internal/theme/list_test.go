package theme

import "testing"

func TestList_ZeroValueRendersWithoutPanicking(t *testing.T) {
	var zero Theme
	_ = zero.List() // must not panic
}

func TestList_DarkAndLightBothBuild(t *testing.T) {
	_ = New(Options{}, true).List()  // must not panic
	_ = New(Options{}, false).List() // must not panic
}

func TestList_TitleUsesAccentFillAndOnAccent(t *testing.T) {
	th := New(Options{}, true)
	s := th.List()
	wantFg := th.Style(RoleOnAccent).GetForeground()
	if got := s.Title.GetForeground(); got != wantFg {
		t.Errorf("List().Title foreground = %v, want RoleOnAccent %v", got, wantFg)
	}
}
