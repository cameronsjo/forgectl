//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package deskweb is `forgectl desk serve`: a small local web page over the
// desk's asks store, so the operator can answer asks from a browser or a
// phone. It shows the open asks by bucket, the snoozed ones, the desk's
// queued scripts (read-only, with their sha256; the page never runs one), and
// the recently answered asks, and takes done / reply / link / snooze.
//
// # Exposure
//
// The server binds loopback only. The CLI refuses any other bind address.
// Phone access goes through `tailscale serve`, which proxies from the tailnet
// to the loopback port, so the page is never on a LAN or public interface.
//
// # Authentication
//
// A per-install token (32 random bytes, <desk>/asks/web.token, mode 0600).
// The URL the server prints carries it in the fragment (#k=TOKEN): a fragment
// is never sent to the server, so it stays out of access logs, proxies and
// Referer. A same-origin script (login.js; the CSP forbids inline script)
// posts it to /session once; the server compares it in constant time and
// sets an HttpOnly, SameSite=Strict session cookie signed with a key derived
// from the token, then the script strips the fragment from the address bar.
// The cookie is stateless, so a restart keeps the session; rotating the token
// (--rotate-token) ends every session.
//
// Rejected: trusting the tailnet identity header (Tailscale-User-Login).
// `tailscale serve` sets it, but on a loopback listener any local process, or
// a page that DNS-rebinds a name to 127.0.0.1, can send the same header, and
// a plain localhost visit carries none at all.
//
// # Request checks
//
//   - Host allowlist (127.0.0.1, localhost, ::1, plus --allow-host names):
//     stops DNS rebinding, and pins r.Host for the Origin check.
//   - Any request a browser labels cross-site is refused.
//   - Every POST also needs Sec-Fetch-Site same-origin or none (or no such
//     header, as from curl), an Origin naming this host when one is sent, and
//     a form token: HMAC(csrf key, session cookie), compared in constant
//     time. The SameSite=Strict cookie is a third layer, not the only one.
//
// # Untrusted text
//
// Ask titles, steps, links and "from" come from agents. Everything goes
// through html/template. Steps are markdown rendered with raw HTML off, then
// a closed bluemonday allowlist (http/https links only, no images). Links are
// shown only when [ask.SafeURL] accepts them. Response flashes are fixed
// strings picked by a code; no request text is reflected.
package deskweb

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/httpsrv"
)

//go:embed assets/page.html
var pageHTML string

//go:embed assets/desk.css
var deskCSS []byte

//go:embed assets/login.js
var loginJS []byte

//go:embed assets/live.js
var liveJS []byte

// maxBody caps a POST body.
const maxBody = 64 << 10

// contentSecurityPolicy forbids every inline script and style, every
// third-party load, framing, and form posts anywhere but here.
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// Script is one desk script shown read-only.
type Script struct {
	Name   string
	Kind   string
	State  string
	What   string
	Why    string
	SHA256 string
}

// Options configure a Server.
type Options struct {
	Store *ask.Store
	// DeskDir is the desk directory, for the read-only script list.
	DeskDir string
	// Token is the install token from LoadToken.
	Token string
	// AllowHosts are extra Host names to accept (a tailscale serve name).
	AllowHosts []string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Scripts lists the desk's scripts; nil reads DeskDir read-only.
	Scripts func() ([]Script, error)
	// Location is the zone snooze times are computed in; nil means Local.
	Location *time.Location
}

// Server is the desk web page.
type Server struct {
	opts Options
	keys keys
	page *template.Template
}

// New builds a Server. It refuses a missing store or a malformed token.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("deskweb: no store")
	}
	if !validToken(opts.Token) {
		return nil, errors.New("deskweb: the token is not 64 hex characters")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Scripts == nil {
		dir := opts.DeskDir
		opts.Scripts = func() ([]Script, error) { return ReadScripts(dir) }
	}
	page, err := template.New("page").Funcs(template.FuncMap{"dict": dict}).Parse(pageHTML)
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, keys: deriveKeys(opts.Token), page: page}, nil
}

// ReadScripts lists the pending and running items of the desk at dir,
// read-only. A directory that is not a desk yet has none.
func ReadScripts(dir string) ([]Script, error) {
	d, err := desk.OpenReadOnly(dir)
	if errors.Is(err, desk.ErrNoDesk) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close() //nolint:errcheck // read-only
	snap, err := d.Scan()
	if err != nil {
		return nil, err
	}
	var out []Script
	for _, it := range slices.Concat(snap.Running, snap.Pending) {
		out = append(out, Script{
			Name: it.Name, Kind: string(it.Kind), State: string(it.State),
			What: it.What, Why: it.Why, SHA256: it.Meta.SHA256,
		})
	}
	return out, nil
}

// Handler is the full handler with every request check applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	css, font := docs.ArtificerAssets()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /session", s.handleSession)
	mux.HandleFunc("POST /respond", s.handleRespond)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /assets/artificer.css", static("text/css; charset=utf-8", css))
	mux.HandleFunc("GET /assets/desk.css", static("text/css; charset=utf-8", deskCSS))
	mux.HandleFunc("GET /assets/login.js", static("text/javascript; charset=utf-8", loginJS))
	mux.HandleFunc("GET /assets/live.js", static("text/javascript; charset=utf-8", liveJS))
	// artificer.css names its fonts relative to itself: assets/fonts/<name>.
	mux.HandleFunc("GET /assets/assets/fonts/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := font(r.PathValue("name"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "font/woff2")
		_, _ = w.Write(b) //nolint:gosec // G705: embedded font bytes, not request-derived
	})
	hosts := slices.Concat(httpsrv.DefaultAllowedHosts, s.opts.AllowHosts)
	return httpsrv.Chain(mux, securityHeaders, httpsrv.HostAllowlist(hosts), checkFetchSite)
}

func static(ctype string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write(body) //nolint:gosec // G705: embedded asset, not request-derived
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// checkFetchSite refuses any request a browser marks cross-site, and any
// POST that is not same-origin by Sec-Fetch-Site and Origin. It runs after
// the Host allowlist, which is what makes comparing Origin with r.Host sound.
func checkFetchSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := strings.ToLower(r.Header.Get("Sec-Fetch-Site"))
		if site == "cross-site" {
			http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "forbidden: not a same-origin request", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !originMatchesHost(o, r.Host) {
				http.Error(w, "forbidden: origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// originMatchesHost reports whether an Origin header names host. The scheme
// may be http or https: behind tailscale serve the browser sees https while
// this server sees plain http, and the Host allowlist already pinned host.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// session returns the request's valid session cookie value, or "".
func (s *Server) session(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil || !s.keys.sessionOK(c.Value, s.opts.Now()) {
		return ""
	}
	return c.Value
}

// secureRequest reports whether the browser reached us over https: directly,
// or through a loopback proxy (tailscale serve) that says so.
func secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.keys.tokenOK(r.PostForm.Get("k")) {
		http.Error(w, "forbidden: wrong token", http.StatusForbidden)
		return
	}
	v, err := s.keys.newSession(s.opts.Now())
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	// Secure is set whenever the browser reached us over https (secureRequest);
	// a plain http://127.0.0.1 visit cannot carry a Secure cookie.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is conditional by design, see above
		Name: cookieName, Value: v, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// flashes are the only response messages the page shows, picked by code.
var flashes = map[string]string{
	"answered":      "Answered.",
	"notified":      "Answered, and the asking agent was told.",
	"notify-failed": "Answered, but telling the asking agent failed. The response is recorded; it can read it with forgectl desk watch --responses.",
	"snoozed":       "Snoozed.",
	"closed":        "That ask was already answered.",
	"invalid":       "That response was not accepted: check the text or link.",
	"not-allowed":   "That ask does not take that kind of response.",
	"gone":          "No such ask.",
	"error":         "Could not record the response; see the server's terminal.",
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = s.page.ExecuteTemplate(w, "login", nil)
		return
	}
	data, err := s.pageData(sess, r.URL.Query().Get("m"))
	if err != nil {
		http.Error(w, "could not read the desk", http.StatusInternalServerError)
		fmt.Fprintf(os.Stderr, "desk serve: read the desk: %v\n", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.page.ExecuteTemplate(w, "page", data); err != nil {
		fmt.Fprintf(os.Stderr, "desk serve: render: %v\n", err)
	}
}

type itemView struct {
	ID, Title, From, Age, Until, Answer string
	Steps                               template.HTML
	Links                               []ask.Link
	Done, Reply, Link, Snooze           bool
}

type bucketView struct {
	Label string
	Items []itemView
}

type pageData struct {
	CSRF       string
	Flash      string
	Buckets    []bucketView
	Open       int
	Snoozed    []itemView
	Scripts    []Script
	ScriptsErr string
	Answered   []itemView
	Version    string
}

func (s *Server) pageData(sess, flash string) (pageData, error) {
	items, err := s.opts.Store.Items()
	if err != nil {
		return pageData{}, err
	}
	now := s.opts.Now()
	open, snoozed, answered := ask.Grouped(items)
	d := pageData{CSRF: s.keys.csrfFor(sess), Flash: flashes[flash]}
	for _, b := range ask.Buckets {
		if len(open[b]) == 0 {
			continue
		}
		bv := bucketView{Label: b.Label()}
		for _, it := range open[b] {
			bv.Items = append(bv.Items, view(it, now))
		}
		d.Open += len(bv.Items)
		d.Buckets = append(d.Buckets, bv)
	}
	for _, it := range snoozed {
		d.Snoozed = append(d.Snoozed, view(it, now))
	}
	for i, it := range answered {
		if i == 10 {
			break
		}
		d.Answered = append(d.Answered, view(it, now))
	}
	d.Scripts, err = s.opts.Scripts()
	if err != nil {
		d.ScriptsErr = "Could not read the desk's scripts."
		fmt.Fprintf(os.Stderr, "desk serve: read scripts: %v\n", err)
	}
	d.Version, _ = s.version()
	return d, nil
}

func view(it ask.Item, now time.Time) itemView {
	v := itemView{
		ID: it.Ask.ID, Title: it.Ask.Title, From: it.Ask.From, Age: age(now.Sub(it.Ask.At)),
		Steps: renderSteps(it.Ask.Steps),
	}
	for _, l := range it.Ask.Links {
		if ask.SafeURL(l.URL) {
			if l.Label == "" {
				l.Label = l.URL
			}
			v.Links = append(v.Links, l)
		}
	}
	if it.State != ask.StateAnswered {
		v.Done, v.Reply, v.Link, v.Snooze = it.Ask.Allows(ask.KindDone), it.Ask.Allows(ask.KindReply), it.Ask.Allows(ask.KindLink), it.Ask.Allows(ask.KindSnooze)
	}
	if it.SnoozedUntil != nil {
		v.Until = it.SnoozedUntil.Local().Format("Mon 15:04")
	}
	if a := it.Answer; a != nil {
		switch a.Kind {
		case ask.KindReply:
			v.Answer = "replied: " + a.Text
		case ask.KindLink:
			v.Answer = "link: " + a.URL
		default:
			v.Answer = string(a.Kind)
		}
		if v.Age = age(now.Sub(a.At)); v.Age != "just now" {
			v.Age += " ago"
		}
	}
	return v
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// snoozeUntil maps a snooze choice to a time.
func snoozeUntil(choice string, now time.Time, loc *time.Location) (time.Time, bool) {
	switch choice {
	case "1h":
		return now.Add(time.Hour), true
	case "4h":
		return now.Add(4 * time.Hour), true
	case "tomorrow":
		t := now.In(loc).AddDate(0, 0, 1)
		return time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, loc), true
	case "week":
		return now.Add(7 * 24 * time.Hour), true
	}
	return time.Time{}, false
}

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.keys.csrfOK(sess, r.PostForm.Get("csrf")) {
		http.Error(w, "forbidden: form token", http.StatusForbidden)
		return
	}
	now := s.opts.Now()
	resp := ask.Response{Ask: r.PostForm.Get("ask"), Kind: ask.Kind(r.PostForm.Get("kind")), Via: "web"}
	switch resp.Kind {
	case ask.KindReply:
		resp.Text = r.PostForm.Get("text")
	case ask.KindLink:
		resp.URL = r.PostForm.Get("url")
	case ask.KindSnooze:
		until, ok := snoozeUntil(r.PostForm.Get("for"), now, s.opts.Location)
		if !ok {
			s.back(w, r, "invalid")
			return
		}
		resp.Until = &until
	}
	_, _, notifyErr, err := s.opts.Store.Respond(r.Context(), resp)
	switch {
	case errors.Is(err, ask.ErrClosed):
		s.back(w, r, "closed")
	case errors.Is(err, ask.ErrNotFound):
		s.back(w, r, "gone")
	case errors.Is(err, ask.ErrNotAllowed):
		s.back(w, r, "not-allowed")
	case errors.Is(err, ask.ErrInvalid):
		s.back(w, r, "invalid")
	case err != nil:
		fmt.Fprintf(os.Stderr, "desk serve: record a response: %v\n", err)
		s.back(w, r, "error")
	case notifyErr != nil:
		fmt.Fprintf(os.Stderr, "desk serve: %v\n", notifyErr)
		s.back(w, r, "notify-failed")
	case resp.Kind == ask.KindSnooze:
		s.back(w, r, "snoozed")
	default:
		s.back(w, r, "answered")
	}
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?m="+code, http.StatusSeeOther)
}

// version is a stamp that changes whenever the asks log or the script list
// does, for live.js.
func (s *Server) version() (string, error) {
	// Writes to a hash.Hash never fail, so the Fprintf errors below are dropped.
	h := sha256.New()
	if fi, err := os.Stat(s.opts.Store.LogPath()); err == nil {
		_, _ = fmt.Fprintf(h, "%d/%d;", fi.Size(), fi.ModTime().UnixNano())
	}
	scripts, err := s.opts.Scripts()
	for _, sc := range scripts {
		_, _ = fmt.Fprintf(h, "%s/%s/%s;", sc.Name, sc.State, sc.SHA256)
	}
	// Snoozes end with time, not with a write: include the minute.
	_, _ = fmt.Fprintf(h, "%d", s.opts.Now().Unix()/60)
	return hex.EncodeToString(h.Sum(nil)[:8]), err
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if s.session(r) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	v, _ := s.version()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"v": v})
}

// Serve runs srv on ln until ctx ends, then shuts down gracefully.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// dict builds a map for passing two values to a sub-template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, errors.New("dict: key is not a string")
		}
		m[k] = kv[i+1]
	}
	return m, nil
}
