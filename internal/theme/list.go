package theme

import "charm.land/bubbles/v2/list"

// List returns a list.Styles built from list.DefaultStyles(t.IsDark()) with
// the title, status, pagination, and dot colours redrawn from t's roles.
func (t Theme) List() list.Styles {
	s := list.DefaultStyles(t.IsDark())

	accent := t.Color(RoleAccent)
	muted := t.Color(RoleMuted)

	s.Title = s.Title.Background(t.Color(RoleAccentFill)).Foreground(t.Color(RoleOnAccent))
	s.StatusBar = s.StatusBar.Foreground(t.Color(RoleMeta))
	s.StatusEmpty = s.StatusEmpty.Foreground(muted)
	s.StatusBarActiveFilter = s.StatusBarActiveFilter.Foreground(accent)
	s.StatusBarFilterCount = s.StatusBarFilterCount.Foreground(muted)
	s.NoItems = s.NoItems.Foreground(muted)
	s.PaginationStyle = s.PaginationStyle.Foreground(muted)
	s.HelpStyle = s.HelpStyle.Foreground(t.Color(RoleMeta))
	s.ActivePaginationDot = s.ActivePaginationDot.Foreground(accent)
	s.InactivePaginationDot = s.InactivePaginationDot.Foreground(muted)
	s.DividerDot = s.DividerDot.Foreground(muted)
	s.DefaultFilterCharacterMatch = s.DefaultFilterCharacterMatch.Foreground(t.Color(RoleOK))

	return s
}
