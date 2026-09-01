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

// artificerTreeJS is the vendored [data-tree] sidenav behavior — roving
// tabindex, clamped arrow navigation, expand/collapse per the WAI-ARIA tree
// pattern. Embedded rather than reimplemented: the sidenav wants a keyboard-
// navigable tree, and this is already vendored and already tested upstream.
//
//go:embed assets/artificer/artificer-tree.js
var artificerTreeJS []byte

// reloadJS is this repo's own live-reload client (not vendored) — the browser
// half of the SSE loop in server.go/watcher.go. Embedded per-file alongside the
// vendored assets above for the same reason they are: only files the UI actually
// links become servable.
//
//go:embed assets/reload.js
var reloadJS []byte

// sidenavFilterJS is the sidenav filter box's behavior. It lives in a file, and
// is embedded and served like every other asset, because the handler's
// Content-Security-Policy sets script-src 'self' — the inline <script> this
// replaced could not run under it.
//
//go:embed assets/sidenav-filter.js
var sidenavFilterJS []byte

// chromaArtificerCSS maps chroma's class-based token output onto the
// Artificer syntax roles (the .tok-* map in artificer.css), replacing the
// generated monokai sheet whose hardcoded palette ignored the theme. Served
// at /assets/chroma.css via ChromaCSS().
//
//go:embed assets/chroma.css
var chromaArtificerCSS []byte

// mermaidJS is vendored mermaid (version, license, and sha256 recorded in
// assets/provenance-mermaid.json). Embedded rather than loaded from a CDN: the
// reader must render a diagram with no network call, because opening a local
// document should not be a third-party request.
//
//go:embed assets/mermaid.min.js
var mermaidJS []byte

// mermaidInitJS configures mermaid from the Artificer --dia-* tokens instead of
// hard-coded colors, and re-renders on theme change.
//
//go:embed assets/mermaid-init.js
var mermaidInitJS []byte

// panZoomJS gives both inline SVG and mermaid-rendered SVG pan/zoom.
//
//go:embed assets/svg-panzoom.js
var panZoomJS []byte

// diagramCSS is the diagram viewport's layout and affordances (no palette — it
// reads Artificer tokens).
//
//go:embed assets/diagram.css
var diagramCSS []byte

//go:embed templates/shell.html.tmpl
var shellTemplateSrc string

// shellTemplate is the one page template the server renders: the
// page-shell chrome (appbar, sidenav, filter box) plus a content slot for
// either a rendered doc or the empty-state. Parsed once at package init —
// a malformed embedded template is a startup-time panic, not a per-request
// failure.
var shellTemplate = template.Must(template.New("shell").Parse(shellTemplateSrc))
