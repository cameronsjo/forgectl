package docs

import (
	_ "embed"
	"html/template"
)

// Vendored via the #219 adherence kit (`artificer vendor`, forgectl#93's
// "Artificer vendoring" requirement) into assets/artificer/ — provenance.json
// there records the exact source version and a sha256 per file; re-run
// `artificer vendor --dest internal/docs/assets/artificer` from the repo
// root to update. Only the two files the docs UI actually links are
// individually embedded (rather than the whole vendored directory), so
// tokens.json/provenance.json/the cheatsheet never become servable over
// HTTP — go:embed's byte slices carry each file's version banner through
// unmodified.
//
//go:embed assets/artificer/artificer.css
var artificerCSS []byte

//go:embed assets/artificer/artificer-theme.js
var artificerThemeJS []byte

//go:embed templates/shell.html.tmpl
var shellTemplateSrc string

// shellTemplate is the one page template the server renders: the
// page-shell chrome (appbar, sidenav, filter box) plus a content slot for
// either a rendered doc or the empty-state. Parsed once at package init —
// a malformed embedded template is a startup-time panic, not a per-request
// failure.
var shellTemplate = template.Must(template.New("shell").Parse(shellTemplateSrc))
