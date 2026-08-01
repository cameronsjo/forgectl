package docs

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// recentCount is how many docs the "Recent" sidenav group shows.
const recentCount = 5

// eventsPath is where the live-reload SSE stream is served. Declared as a
// constant because the embedded reload client (assets/reload.js) must agree
// with it.
const eventsPath = "/events"

// reloadMessage is the single SSE payload the server ever sends. The client
// treats any message as "re-read this page", so the content carries no
// information the client acts on differently — it exists to be legible in
// devtools and in a curl of the endpoint.
const reloadMessage = "reload"

// locatePath answers "which URL serves the file at this path?" for
// `forgectl docs open`.
const locatePath = "/api/locate"

// contentSecurityPolicy is sent on EVERY response this handler produces, not
// just the rendered shell. The reader executes markdown the operator wrote —
// and, increasingly, markdown Claude wrote for them — so the sanitizer in
// render.go is the first line and this is the second: whatever the sanitizer
// were ever to let through still cannot reach off-origin.
//
// Directive by directive, because each is a decision and not a copied
// boilerplate line:
//
//   - default-src 'self' — the floor. Every fetch type without its own
//     directive below inherits same-origin-only.
//   - script-src 'self' — no 'unsafe-inline' (the shell has no inline
//     <script>; the sidenav filter was moved to assets/sidenav-filter.js
//     precisely so this could stay clean) and, deliberately, NO 'unsafe-eval'.
//     The vendored mermaid bundle contains exactly two Function-constructor
//     sites, both the `Function("return this")` globalThis polyfill, which
//     short-circuits on a real `globalThis` before ever constructing anything
//     in any browser this reader targets. Nothing here needs eval, so nobody
//     should add 'unsafe-eval' back "just in case" — the correct response to a
//     CSP eval violation is to find what started evaluating code.
//   - style-src 'self' 'unsafe-inline' — FORCED, from two independent
//     directions, which is why neither can be fixed away alone. The shell
//     template carries layout style= attributes, and mermaid injects a <style>
//     element per rendered diagram with theme-computed CSS: it exposes no nonce
//     hook, and the content varies with the Artificer tokens, so no hash is
//     knowable ahead of the render. Note what this does NOT reopen: the classic
//     CSS-based exfiltration (a selector whose background-image URL leaks what
//     it matched) is closed by img-src 'self', not by style-src.
//   - img-src 'self' data: — the one behavior CHANGE in this policy. A doc
//     referencing a remote image stops loading it (data: URIs still work, which
//     is what inline SVG and mermaid need). No doc in this repo does that, and
//     it closes the tracking-beacon path render.go documents as open by design
//     under bluemonday's UGCPolicy: opening a document should not tell a third
//     party you opened it.
//   - font-src 'self' — named explicitly rather than left to default-src so a
//     later embedded font works without a policy edit, and so a remote font
//     (another beacon, and a fingerprinting surface) cannot slip in with it.
//   - connect-src 'self' — the live-reload EventSource is same-origin; nothing
//     else should be opening sockets.
//   - object-src 'none' — no <object>/<embed>; legacy plugin content is a
//     script-execution path this reader has no use for.
//   - base-uri 'none' — a <base> tag rewrites every relative URL on the page,
//     which would redirect the asset loads above. This does NOT fall back to
//     default-src, so omitting it would leave it unrestricted.
//   - form-action 'none' — the reader has no forms; an injected one cannot
//     POST anywhere. Also does NOT fall back to default-src.
//   - frame-ancestors 'none' — nothing may frame the reader, so a page cannot
//     clickjack a loopback origin it is otherwise unable to read.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// locateResponse is the locate endpoint's payload. It deliberately does NOT
// echo an absolute path: Doc.AbsPath is documented as never being sent to a
// client, and the caller already knows the path it asked about.
type locateResponse struct {
	Root  string `json:"root"`
	Rel   string `json:"rel"`
	Title string `json:"title"`
}

// handleLocate maps an absolute filesystem path to the (root, rel) pair whose
// URL serves it, or 404 when the running index does not contain that file.
//
// Why the server answers this instead of the client computing it: the index is
// the single source of truth for what is servable — root labels (including the
// numeric disambiguation two same-named roots get), the walk's directory
// exclusions, and the single-file-root restriction all live here. A client that
// built URLs by string-joining a path onto a root would happily produce a URL
// for a file the server then refuses to serve, and would drift from the
// exclusion rules the moment either side changed. It also reflects live reload
// for free, because it reads the current index.
//
// On disclosure: this tells the caller whether a given path is indexed. That is
// membership only — never content, and never a path the caller did not already
// supply — and the caller is the operator on the same machine, reaching a
// loopback server that requires a bearer token whenever it is bound anywhere
// else. It hands a stranger nothing they could not learn by requesting the doc
// URL directly.
func handleLocate(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("path")
		if raw == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}

		// Canonicalize the same way the indexer did, so the comparison is
		// like-for-like. A path that cannot be resolved is simply not indexed.
		resolved, err := filepath.EvalSymlinks(raw)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		resolved = filepath.Clean(resolved)

		doc, ok := store.Current().FindByAbsPath(resolved)
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(locateResponse{
			Root:  doc.RootLabel,
			Rel:   doc.RelPath,
			Title: doc.Title,
		}); err != nil {
			slog.Debug("docs: encoding locate response failed.", "error", err)
		}
	}
}

// NewHandler builds the complete `forgectl docs serve` HTTP handler over a
// Store holding the current Index: the doc-shell page, per-doc routes, the
// live-reload stream, the locate endpoint, and the embedded static assets. It is
// the docs package's sole exported handler constructor — security POLICY (Host
// allowlist, bearer token, anything a caller might configure differently) is the
// caller's job via internal/httpsrv middleware wrapped around this handler.
// The securityHeaders pair is different: X-Content-Type-Options and
// Content-Security-Policy are fixed, no-config hardening defaults for every
// response this handler ever produces — there is no deployment of this handler
// that wants them off, and the CSP is coupled to the markup and assets this
// package itself emits — so they are applied here rather than pushed out as an
// opt-in caller concern.
//
// A nil events Broker disables live reload — the stream endpoint 404s and
// nothing else changes.
func NewHandler(store *Store, events *Broker) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /assets/artificer.css", serveStaticCSS(artificerCSS))
	mux.HandleFunc("GET /assets/artificer-theme.js", serveStaticJS(artificerThemeJS))
	mux.HandleFunc("GET /assets/reload.js", serveStaticJS(reloadJS))
	mux.HandleFunc("GET /assets/mermaid.min.js", serveStaticJS(mermaidJS))
	mux.HandleFunc("GET /assets/mermaid-init.js", serveStaticJS(mermaidInitJS))
	mux.HandleFunc("GET /assets/svg-panzoom.js", serveStaticJS(panZoomJS))
	mux.HandleFunc("GET /assets/artificer-tree.js", serveStaticJS(artificerTreeJS))
	mux.HandleFunc("GET /assets/sidenav-filter.js", serveStaticJS(sidenavFilterJS))
	mux.HandleFunc("GET /assets/chroma.css", serveStaticCSS(ChromaCSS()))
	mux.HandleFunc("GET /assets/diagram.css", serveStaticCSS(diagramCSS))

	mux.HandleFunc("GET "+eventsPath, handleEvents(events))
	mux.HandleFunc("GET "+locatePath, handleLocate(store))
	mux.HandleFunc("GET /doc/{root}/{rest...}", handleDoc(store))
	mux.HandleFunc("GET /{$}", handleIndexRoot(store))

	return securityHeaders(mux)
}

// handleEvents streams reload notifications to one browser as Server-Sent
// Events. It returns — releasing the connection — as soon as the client
// disconnects (request context canceled) or the Broker is closed at shutdown.
//
// Flushing goes through http.NewResponseController rather than a
// w.(http.Flusher) type assertion. The assertion form is the older idiom and it
// is brittle: any middleware that wraps the ResponseWriter without forwarding
// Flush turns SSE into a stream that buffers forever, and the failure mode is a
// page that simply never updates. ResponseController unwraps through wrappers.
func handleEvents(events *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if events == nil {
			http.NotFound(w, r) // live reload not enabled for this server
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		// Defense in depth against a reverse proxy buffering the stream into
		// uselessness; harmless when nothing is proxying.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// Subscribe BEFORE writing the initial frame, not after. A Publish that
		// landed between the flush and the subscription would be delivered to
		// nobody for this client — the channel would not exist yet — so the
		// browser would sit on an open stream having silently missed the reload
		// that prompted it. Subscribing first cannot fail and costs nothing, and
		// it makes the window zero rather than small.
		sub, cancel := events.Subscribe()
		defer cancel()

		rc := http.NewResponseController(w)
		// A comment frame proves the stream is live before any file changes, so
		// EventSource fires onopen and the client stops waiting.
		if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			slog.Debug("docs: SSE stream could not be flushed; live reload unavailable for this client.", "error", err)
			return
		}

		for {
			select {
			case <-r.Context().Done():
				return // browser navigated away or closed the tab
			case msg, ok := <-sub:
				if !ok {
					return // broker closed: server is shutting down
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}
			}
		}
	}
}

// securityHeaders sets the fixed, no-configuration response headers every
// response this handler produces carries. It wraps the whole mux rather than
// the shell renderer alone, so an asset, a JSON payload, and a 404 are covered
// on the same terms as a rendered doc — a header that protects only the page a
// developer remembered is the shape that fails later.
//
// X-Content-Type-Options: nosniff stops a browser from MIME-sniffing a response
// body into a different content type than the Content-Type header declares (the
// classic vector is a browser deciding a text/plain or text/css response is
// actually HTML/JS and executing it) — irrelevant for the sanitized doc HTML
// this handler serves deliberately, but free insurance against a future
// response type this handler doesn't anticipate today.
//
// Content-Security-Policy is the second layer behind render.go's sanitizer; see
// contentSecurityPolicy above for why each directive is what it is.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
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
func handleIndexRoot(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		renderShell(w, store.Current(), pageContext{})
	}
}

// handleDoc resolves {root}/{rest...} through the Index's traversal chain
// and renders the matched doc inside the shell. Every failure path — unknown
// root, traversal attempt, disallowed extension, an unreadable file — maps
// to the same 404; the docs-serving surface never distinguishes the reason
// to the client (forgectl#93's stated posture: a stranger on the loopback
// interface gets no hints about why a path didn't resolve).
func handleDoc(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		root := r.PathValue("root")
		rest := r.PathValue("rest")

		// Load the index ONCE per request. Re-reading store.Current() later in
		// this handler would let a live-reload swap land mid-response, so the
		// doc could be resolved against one tree and the sidenav built from
		// another.
		idx := store.Current()

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
