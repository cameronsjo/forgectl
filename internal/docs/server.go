package docs

import (
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
)

// recentCount is how many docs the "Recent" sidenav group shows.
const recentCount = 5

// NewHandler builds the complete `forgectl docs serve` HTTP handler over an
// already-built Index: the doc-shell page, per-doc routes, and the two
// embedded static assets. It is the docs package's sole exported handler
// constructor — security policy (Host allowlist, bearer token) is the
// caller's job via internal/httpsrv middleware wrapped around this handler,
// not something this package decides for itself.
func NewHandler(idx *Index) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /assets/artificer.css", serveStaticCSS(artificerCSS))
	mux.HandleFunc("GET /assets/artificer-theme.js", serveStaticJS(artificerThemeJS))
	mux.HandleFunc("GET /assets/chroma.css", serveStaticCSS(ChromaCSS()))

	mux.HandleFunc("GET /doc/{root}/{rest...}", handleDoc(idx))
	mux.HandleFunc("GET /{$}", handleIndexRoot(idx))

	return mux
}

func serveStaticCSS(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // #93 PR1 has no cache-busting scheme yet; correctness over speed
		w.Write(body)
	}
}

func serveStaticJS(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	}
}

// handleIndexRoot renders the shell with the empty-state content — "/"
// itself never resolves to a specific doc.
func handleIndexRoot(idx *Index) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderShell(w, idx, pageContext{})
	}
}

// handleDoc resolves {root}/{rest...} through the Index's traversal chain
// and renders the matched doc inside the shell. Every failure path — unknown
// root, traversal attempt, disallowed extension, an unreadable file — maps
// to the same 404; the docs-serving surface never distinguishes the reason
// to the client (forgectl#93's stated posture: a stranger on the loopback
// interface gets no hints about why a path didn't resolve).
func handleDoc(idx *Index) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		root := r.PathValue("root")
		rest := r.PathValue("rest")

		absPath, err := idx.Resolve(root, rest)
		if err != nil {
			slog.Debug("docs: request did not resolve to a servable file.", "root", root, "rest", rest, "error", err)
			http.NotFound(w, r)
			return
		}

		source, err := os.ReadFile(absPath)
		if err != nil {
			slog.Warn("docs: resolved path could not be read.", "error", err)
			http.NotFound(w, r)
			return
		}

		rendered, err := Render(source)
		if err != nil {
			slog.Error("docs: markdown render failed.", "root", root, "rest", rest, "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}

		doc, _ := idx.Find(root, rest)
		renderShell(w, idx, pageContext{
			CurrentRoot: root,
			CurrentRel:  rest,
			DocTitle:    doc.Title,
			Content:     template.HTML(rendered), //nolint:gosec // rendered is bluemonday-sanitized in Render
		})
	}
}

// pageContext is what a request handler fills in before calling
// renderShell; renderShell itself owns turning it (plus the Index) into the
// template's data shape.
type pageContext struct {
	CurrentRoot string
	CurrentRel  string
	DocTitle    string
	Content     template.HTML
}

// shellData is the template's data contract (templates/shell.html.tmpl).
type shellData struct {
	DocTitle string
	Content  template.HTML
	Groups   []sidenavGroup
}

type sidenavGroup struct {
	Root string
	Docs []sidenavLink
}

type sidenavLink struct {
	Href       string
	Title      string
	FilterText string
	Current    bool
}

func renderShell(w http.ResponseWriter, idx *Index, ctx pageContext) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := shellData{
		DocTitle: ctx.DocTitle,
		Content:  ctx.Content,
		Groups:   buildGroups(idx, ctx.CurrentRoot, ctx.CurrentRel),
	}
	if err := shellTemplate.Execute(w, data); err != nil {
		slog.Error("docs: template execution failed.", "error", err)
	}
}

// buildGroups assembles the sidenav's data: a "Recent" group of the
// most-recently-modified docs across every root, followed by one group per
// root listing its docs alphabetically by RelPath.
func buildGroups(idx *Index, currentRoot, currentRel string) []sidenavGroup {
	var groups []sidenavGroup

	all := idx.List() // already most-recently-modified first
	if n := min(recentCount, len(all)); n > 0 {
		groups = append(groups, sidenavGroup{
			Root: "Recent",
			Docs: toLinks(all[:n], currentRoot, currentRel),
		})
	}

	for _, root := range idx.Roots() {
		var docs []Doc
		for _, d := range all {
			if d.RootLabel == root.Label {
				docs = append(docs, d)
			}
		}
		sort.Slice(docs, func(i, j int) bool { return docs[i].RelPath < docs[j].RelPath })
		groups = append(groups, sidenavGroup{
			Root: root.Label,
			Docs: toLinks(docs, currentRoot, currentRel),
		})
	}

	return groups
}

func toLinks(docs []Doc, currentRoot, currentRel string) []sidenavLink {
	links := make([]sidenavLink, 0, len(docs))
	for _, d := range docs {
		links = append(links, sidenavLink{
			Href:       "/doc/" + d.RootLabel + "/" + d.RelPath,
			Title:      d.Title,
			FilterText: strings.ToLower(d.Title + " " + d.RelPath),
			Current:    d.RootLabel == currentRoot && d.RelPath == currentRel,
		})
	}
	return links
}
