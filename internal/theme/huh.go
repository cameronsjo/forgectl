package theme

import (
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// Huh returns a huh.Theme that ignores the isDark argument huh v2 passes it
// and resolves from t.IsDark() instead. This is deliberate, not an oversight:
// huh v2 never issues a background-colour query for a standalone
// Form.Run() (form.go never calls tea.RequestBackgroundColor outside a full
// Bubble Tea program), so the argument huh hands ThemeFunc is always false —
// trusting it would render every standalone form light regardless of the
// real terminal.
func (t Theme) Huh() huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles {
		s := huh.ThemeBase(t.IsDark())

		accent := t.Color(RoleAccent)
		s.Focused.Title = s.Focused.Title.Foreground(accent)
		s.Focused.NoteTitle = s.Focused.NoteTitle.Foreground(accent)
		s.Focused.Directory = s.Focused.Directory.Foreground(accent)
		s.Focused.SelectSelector = s.Focused.SelectSelector.Foreground(accent)
		s.Focused.MultiSelectSelector = s.Focused.MultiSelectSelector.Foreground(accent)
		s.Focused.SelectedPrefix = s.Focused.SelectedPrefix.Foreground(accent)
		s.Focused.NextIndicator = s.Focused.NextIndicator.Foreground(accent)
		s.Focused.PrevIndicator = s.Focused.PrevIndicator.Foreground(accent)
		s.Focused.TextInput.Prompt = s.Focused.TextInput.Prompt.Foreground(accent)
		s.Focused.TextInput.Cursor = s.Focused.TextInput.Cursor.Foreground(accent)

		s.Focused.Description = s.Focused.Description.Foreground(t.Color(RoleMeta))

		muted := t.Color(RoleMuted)
		s.Focused.Option = s.Focused.Option.Foreground(t.Color(RoleFg))
		s.Focused.UnselectedOption = s.Focused.UnselectedOption.Foreground(muted)
		s.Focused.UnselectedPrefix = s.Focused.UnselectedPrefix.Foreground(muted)
		s.Focused.SelectedOption = s.Focused.SelectedOption.Foreground(t.Color(RoleOK))

		danger := t.Color(RoleDanger)
		s.Focused.ErrorIndicator = s.Focused.ErrorIndicator.Foreground(danger)
		s.Focused.ErrorMessage = s.Focused.ErrorMessage.Foreground(danger)

		s.Focused.FocusedButton = s.Focused.FocusedButton.
			Foreground(t.Color(RoleOnAccent)).
			Background(t.Color(RoleAccentFill))
		s.Focused.BlurredButton = s.Focused.BlurredButton.
			Foreground(muted).
			Background(t.Color(RoleSurfaceRaised))

		s.Focused.TextInput.Placeholder = s.Focused.TextInput.Placeholder.Foreground(t.Color(RoleDim))

		s.Group.Title = s.Focused.Title
		s.Group.Description = s.Focused.Description

		// Blurred is rebuilt last, from the now-fully-styled Focused, mirroring
		// every built-in huh theme (ThemeCharm, ThemeDracula, …): a Blurred
		// field set before this point would be overwritten here, not layered.
		s.Blurred = s.Focused
		s.Blurred.Base = s.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
		s.Blurred.Card = s.Blurred.Base
		s.Blurred.NextIndicator = lipgloss.NewStyle()
		s.Blurred.PrevIndicator = lipgloss.NewStyle()

		return s
	})
}
