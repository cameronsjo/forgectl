//go:build unix

package deskweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
)

const testHost = "127.0.0.1:8765"

type stubNotifier struct {
	mu    sync.Mutex
	calls []string
	// ctxErrs records each notify context's error at call time.
	ctxErrs []error
}

func (s *stubNotifier) Notify(ctx context.Context, addr, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, addr+" "+text)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return nil
}

type fixture struct {
	t       *testing.T
	dir     string
	store   *ask.Store
	srv     *Server
	h       http.Handler
	token   string
	notify  *stubNotifier
	now     time.Time
	cookie  *http.Cookie
	askID   string
	logPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), notify: &stubNotifier{}, now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	var err error
	f.store, err = ask.Open(f.dir, ask.WithNotifier(f.notify), ask.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	f.token, err = LoadToken(f.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	f.srv, err = New(Options{
		Store: f.store, DeskDir: f.dir, Token: f.token, Port: 18765, Now: clock, Location: time.UTC,
		Scripts: func() ([]Script, error) {
			return []Script{{Name: "01-merge", Kind: "script", State: "waiting", What: "Merge <b>it</b>", SHA256: strings.Repeat("ab", 32)}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.h = f.srv.Handler()
	a, err := f.store.Add(ask.Ask{Title: "Rotate the SSH key", Steps: "1. Open the app\n2. Save", Bucket: ask.BucketNow, From: "worker-a", Notify: "t1k"})
	if err != nil {
		t.Fatal(err)
	}
	f.askID = a.ID
	f.logPath = f.store.LogPath()
	return f
}

func (f *fixture) do(method, path string, form url.Values, hdr map[string]string, withCookie bool) *httptest.ResponseRecorder {
	f.t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequestWithContext(f.t.Context(), method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequestWithContext(f.t.Context(), method, path, nil)
	}
	req.Host = testHost
	req.RemoteAddr = "127.0.0.1:50000"
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if withCookie && f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) login() {
	f.t.Helper()
	rec := f.do("POST", "/session", url.Values{"k": {f.token}}, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost}, false)
	if rec.Code != http.StatusNoContent {
		f.t.Fatalf("login: status %d, body %q", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == f.srv.cookieName() {
			f.cookie = c
		}
	}
	if f.cookie == nil {
		f.t.Fatal("login set no session cookie")
	}
}

func (f *fixture) csrf() string {
	return f.srv.keys.csrfFor(f.cookie.Value)
}

func (f *fixture) logSize() int64 {
	fi, err := os.Stat(f.logPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return fi.Size()
}

func TestAuth_NoSessionShowsLoginWithoutAskText(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/", "/api/version"} {
		rec := f.do("GET", path, nil, nil, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no session: status %d, want 401", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "Rotate the SSH key") || strings.Contains(rec.Body.String(), f.askID) {
			t.Errorf("GET %s with no session leaked ask text", path)
		}
	}
	// A forged cookie is no session.
	f.cookie = &http.Cookie{Name: f.srv.cookieName(), Value: "v1.99999999999.00.deadbeef"} //nolint:gosec // G124: a forged client-side cookie, never set by a server
	if rec := f.do("GET", "/", nil, nil, true); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie: status %d, want 401", rec.Code)
	}
}

func TestAuth_TokenExchange(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		k    string
		want int
	}{
		{"wrong token", strings.Repeat("0", 64), http.StatusForbidden},
		{"empty token", "", http.StatusForbidden},
		{"token prefix", f.token[:32], http.StatusForbidden},
		{"right token", f.token, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do("POST", "/session", url.Values{"k": {tc.k}}, map[string]string{"Sec-Fetch-Site": "same-origin"}, false)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			var got *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == f.srv.cookieName() {
					got = c
				}
			}
			if (got != nil) != (tc.want == http.StatusNoContent) {
				t.Fatalf("cookie set = %v, want %v", got != nil, tc.want == http.StatusNoContent)
			}
			if got != nil && (!got.HttpOnly || got.SameSite != http.SameSiteStrictMode) {
				t.Fatalf("cookie is not HttpOnly SameSite=Strict: %+v", got)
			}
		})
	}
	f.login()
	rec := f.do("GET", "/", nil, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Rotate the SSH key") {
		t.Fatalf("GET / after login: status %d", rec.Code)
	}
}

func TestAuth_ExpiredSessionAndRotatedToken(t *testing.T) {
	f := newFixture(t)
	f.login()
	f.now = f.now.Add(sessionTTL + time.Minute)
	if rec := f.do("GET", "/", nil, nil, true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: status %d, want 401", rec.Code)
	}
	f.now = f.now.Add(-sessionTTL - time.Minute)
	// A server on a rotated token refuses the old cookie.
	tok, err := LoadToken(f.dir, true)
	if err != nil {
		t.Fatal(err)
	}
	srv2, err := New(Options{Store: f.store, DeskDir: f.dir, Token: tok, Now: func() time.Time { return f.now }, Scripts: func() ([]Script, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.h = srv2.Handler()
	if rec := f.do("GET", "/", nil, nil, true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie from before rotation: status %d, want 401", rec.Code)
	}
}

func TestHostAllowlist(t *testing.T) {
	f := newFixture(t)
	f.login()
	cases := []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:8765", true},
		{"localhost:8765", true},
		{"[::1]:8765", true},
		{"evil.example:8765", false},
		{"127.0.0.1.nip.io", false},
	}
	for _, tc := range cases {
		rec := f.do("GET", "/", nil, map[string]string{"Host": tc.host}, true)
		if got := rec.Code == http.StatusOK; got != tc.ok {
			t.Errorf("Host %s: status %d, ok=%v want %v", tc.host, rec.Code, got, tc.ok)
		}
	}
	// A bad host cannot log in either.
	rec := f.do("POST", "/session", url.Values{"k": {f.token}}, map[string]string{"Host": "evil.example"}, false)
	if rec.Code != http.StatusForbidden {
		t.Errorf("login from a bad host: status %d, want 403", rec.Code)
	}
}

func TestCSRF_RefusedPostsAppendNothing(t *testing.T) {
	f := newFixture(t)
	f.login()
	good := map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost}
	form := func(csrf string) url.Values {
		return url.Values{"csrf": {csrf}, "ask": {f.askID}, "kind": {"done"}}
	}
	cases := []struct {
		name   string
		form   url.Values
		hdr    map[string]string
		cookie bool
	}{
		{"no csrf field", url.Values{"ask": {f.askID}, "kind": {"done"}}, good, true},
		{"wrong csrf", form(strings.Repeat("0", 64)), good, true},
		{"csrf for another session", form(f.srv.keys.csrfFor("v1.1.2.3")), good, true},
		{"cross-site fetch", form(f.csrf()), map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"same-site fetch", form(f.csrf()), map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"foreign origin", form(f.csrf()), map[string]string{"Origin": "https://evil.example"}, true},
		{"null origin", form(f.csrf()), map[string]string{"Origin": "null"}, true},
		{"no session", form(f.csrf()), good, false},
	}
	before := f.logSize()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do("POST", "/respond", tc.form, tc.hdr, tc.cookie)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 403 or 401", rec.Code)
			}
		})
	}
	if after := f.logSize(); after != before {
		t.Fatalf("a refused POST appended to the log (%d -> %d bytes)", before, after)
	}
	if len(f.notify.calls) != 0 {
		t.Fatalf("a refused POST notified: %v", f.notify.calls)
	}
}

func TestRespond_HappyPathAppendsAndNotifies(t *testing.T) {
	f := newFixture(t)
	f.login()
	hdr := map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost}
	rec := f.do("POST", "/respond", url.Values{"csrf": {f.csrf()}, "ask": {f.askID}, "kind": {"reply"}, "text": {"keys done"}}, hdr, true)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?m=answered" {
		t.Fatalf("respond: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	items, err := f.store.Items()
	if err != nil {
		t.Fatal(err)
	}
	it, _ := ask.Find(items, f.askID)
	if it.State != ask.StateAnswered || it.Answer.Text != "keys done" || it.Answer.Via != "web" || it.Answer.Origin != ask.OriginOperator {
		t.Fatalf("ask after reply: %+v", it)
	}
	if len(f.notify.calls) != 1 || !strings.HasPrefix(f.notify.calls[0], "t1k DESK-RESPONSE "+f.askID+" reply -- answered by the operator in the desk web page") ||
		!strings.Contains(f.notify.calls[0], "reply “keys done”") {
		t.Fatalf("notify calls: %v", f.notify.calls)
	}
	// Answering again is refused with a flash, and appends nothing.
	before := f.logSize()
	rec = f.do("POST", "/respond", url.Values{"csrf": {f.csrf()}, "ask": {f.askID}, "kind": {"done"}}, hdr, true)
	if rec.Header().Get("Location") != "/?m=closed" || f.logSize() != before {
		t.Fatalf("second answer: location %q", rec.Header().Get("Location"))
	}
}

func TestRespond_SnoozeAndAllowedKinds(t *testing.T) {
	f := newFixture(t)
	f.login()
	hdr := map[string]string{"Sec-Fetch-Site": "same-origin"}
	only, err := f.store.Add(ask.Ask{Title: "Just tick it", Bucket: ask.BucketSoon, Allow: []ask.Kind{ask.KindDone}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"kind not allowed", url.Values{"ask": {only.ID}, "kind": {"reply"}, "text": {"x"}}, "/?m=not-allowed"},
		{"unknown snooze", url.Values{"ask": {f.askID}, "kind": {"snooze"}, "for": {"forever"}}, "/?m=invalid"},
		{"javascript link", url.Values{"ask": {f.askID}, "kind": {"link"}, "url": {"javascript:alert(1)"}}, "/?m=invalid"},
		{"no such ask", url.Values{"ask": {"ask-00000000"}, "kind": {"done"}}, "/?m=gone"},
		{"snooze tomorrow", url.Values{"ask": {f.askID}, "kind": {"snooze"}, "for": {"tomorrow"}}, "/?m=snoozed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.form.Set("csrf", f.csrf())
			rec := f.do("POST", "/respond", tc.form, hdr, true)
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Fatalf("location %q, want %q (status %d)", got, tc.want, rec.Code)
			}
		})
	}
	items, err := f.store.Items()
	if err != nil {
		t.Fatal(err)
	}
	it, _ := ask.Find(items, f.askID)
	want := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if it.State != ask.StateSnoozed || !it.SnoozedUntil.Equal(want) {
		t.Fatalf("snoozed ask: state %s until %v, want snoozed until %v", it.State, it.SnoozedUntil, want)
	}
	// The page renders only the allowed forms for the done-only ask.
	body := f.do("GET", "/", nil, nil, true).Body.String()
	card := body[strings.Index(body, "Just tick it"):]
	card = card[:strings.Index(card, "</div>\n")]
	if strings.Contains(card, `value="reply"`) || !strings.Contains(card, `value="done"`) {
		t.Fatalf("done-only ask renders other forms:\n%s", card)
	}
}

// writeRaw appends a record straight to the log, as a hand edit or another
// tool might, bypassing the store's checks.
func writeRaw(t *testing.T, path string, rec ask.Record) {
	t.Helper()
	rec.V = 1
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the test's own temp log
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRender_HostileAskTextIsInert(t *testing.T) {
	f := newFixture(t)
	f.login()
	_, err := f.store.Add(ask.Ask{
		Title:  `<script>alert(1)</script>`,
		Steps:  "<img src=x onerror=alert(2)>\n\n[click](javascript:alert(3))\n\n<a href=\"javascript:alert(4)\">raw</a>\n\n![px](https://tracker.example/p.gif)\n\n[ok](https://example.com/ok)",
		Bucket: ask.BucketNow,
		From:   `"><svg onload=alert(5)>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A link the store would refuse, written straight into the log.
	writeRaw(t, f.logPath, ask.Record{Ask: &ask.Ask{
		ID: "ask-deadbeef", At: f.now, Title: "raw", Bucket: ask.BucketNow, Allow: ask.Kinds,
		Links: []ask.Link{{Label: "bad", URL: "javascript:alert(6)"}, {Label: "data", URL: "data:text/html,<script>alert(7)</script>"}},
	}})
	body := f.do("GET", "/", nil, nil, true).Body.String()
	for _, bad := range []string{">bad</a>", ">data</a>", "ZgotmplZ", "<script>alert", "onerror", "javascript:", "<svg", "<img", "data:text/html", "tracker.example"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	for _, good := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;svg onload=alert(5)&gt;", `href="https://example.com/ok"`} {
		if !strings.Contains(body, good) {
			t.Errorf("page lacks %q", good)
		}
	}
}

func TestRender_StepsMarkdown(t *testing.T) {
	cases := []struct {
		src      string
		has, not []string
	}{
		{"**bold** and `code`", []string{"<strong>bold</strong>", "<code>code</code>"}, nil},
		{"<b onclick=x>raw</b>", nil, []string{"<b", "onclick"}},
		{"[x](javascript:alert(1))", nil, []string{"javascript", "href"}},
		{"[x](/relative)", nil, []string{"href"}},
		{"[x](https://e.example/a)", []string{`href="https://e.example/a"`, `rel="nofollow noreferrer noopener"`, `target="_blank"`}, nil},
		{"![i](https://e.example/i.png)", nil, []string{"<img"}},
	}
	for _, tc := range cases {
		got := string(renderSteps(tc.src))
		for _, h := range tc.has {
			if !strings.Contains(got, h) {
				t.Errorf("renderSteps(%q) = %q, lacks %q", tc.src, got, h)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("renderSteps(%q) = %q, has %q", tc.src, got, n)
			}
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/", nil, nil, false)
	for k, want := range map[string]string{
		"Content-Security-Policy": contentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
		"Cache-Control":           "no-store",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(contentSecurityPolicy, "script-src 'self'") || strings.Contains(contentSecurityPolicy, "unsafe") {
		t.Errorf("CSP allows inline script: %s", contentSecurityPolicy)
	}
}

func TestPage_ShowsScriptsReadOnly(t *testing.T) {
	f := newFixture(t)
	f.login()
	body := f.do("GET", "/", nil, nil, true).Body.String()
	if !strings.Contains(body, "sha256="+strings.Repeat("ab", 32)) || !strings.Contains(body, "Merge &lt;b&gt;it&lt;/b&gt;") {
		t.Fatalf("scripts section missing or unescaped")
	}
	scripts := body[strings.Index(body, "Scripts on the desk"):]
	if strings.Contains(scripts[:strings.Index(scripts, "</section>")], "<form") {
		t.Fatal("the scripts section offers a form")
	}
}

func TestSecureCookieOnlyBehindHTTPSLoopbackProxy(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		remote, proto string
		secure        bool
	}{
		{"127.0.0.1:1", "", false},
		{"127.0.0.1:1", "https", true},
		{"[::1]:1", "https", true},
		{"10.0.0.5:1", "https", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/session", strings.NewReader(url.Values{"k": {f.token}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host, req.RemoteAddr = testHost, tc.remote
		if tc.proto != "" {
			req.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		cs := rec.Result().Cookies()
		if len(cs) != 1 || cs[0].Secure != tc.secure {
			t.Errorf("remote %s proto %q: cookies %+v, want Secure=%v", tc.remote, tc.proto, cs, tc.secure)
		}
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ask.SubDir), 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := LoadToken(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadToken(dir, false)
	if err != nil || a != b {
		t.Fatalf("second load = %q, %v; want the same token", b, err)
	}
	p := filepath.Join(dir, ask.SubDir, TokenName)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v, %v", fi.Mode(), err)
	}
	c, err := LoadToken(dir, true)
	if err != nil || c == a {
		t.Fatalf("rotate kept the token")
	}
	if err := os.Chmod(p, 0o644); err != nil { //nolint:gosec // G302: staging a world-readable token on purpose
		t.Fatal(err)
	}
	if _, err := LoadToken(dir, false); err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", p); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(dir, false); err == nil {
		t.Fatal("a symlinked token file was followed")
	}
}

// The sanitizer is its own layer: goldmark already blanks a javascript: link
// in markdown, so this feeds the sanitizer HTML directly.
func TestSanitizer_LinkSchemes(t *testing.T) {
	cases := map[string]bool{
		`<a href="https://e.example/">x</a>`:  true,
		`<a href="http://e.example/">x</a>`:   true,
		`<a href="javascript:alert(1)">x</a>`: false,
		`<a href="data:text/html,hi">x</a>`:   false,
		`<a href="vbscript:x">x</a>`:          false,
		`<a href="/relative">x</a>`:           false,
		`<a href="//proto.relative/">x</a>`:   false,
	}
	for in, keep := range cases {
		got := sanitizer.Sanitize(in)
		if strings.Contains(got, "href") != keep {
			t.Errorf("Sanitize(%q) = %q, keep href = %v", in, got, keep)
		}
	}
}

// TestPageReferrerPolicyKeepsTheOrigin pins the fix for real browsers: under a
// no-referrer policy (header or meta tag) a browser sends "Origin: null" on a
// form POST, which the fetch-site check refuses, so no answer would land.
func TestPageReferrerPolicyKeepsTheOrigin(t *testing.T) {
	if strings.Contains(pageHTML, "no-referrer") {
		t.Fatal("page.html sets a no-referrer policy; form POSTs would carry Origin: null")
	}
}

// TestSession_BoundToItsPort: a cookie this token signed for another port
// does not open this server.
func TestSession_BoundToItsPort(t *testing.T) {
	f := newFixture(t)
	other, err := New(Options{Store: f.store, DeskDir: f.dir, Token: f.token, Port: 18766})
	if err != nil {
		t.Fatal(err)
	}
	v, err := other.keys.newSession(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if f.srv.keys.sessionOK(v, f.now) {
		t.Fatal("a session signed for port 18766 opened the server on 18765")
	}
	if !other.keys.sessionOK(v, f.now) {
		t.Fatal("the session does not open its own port")
	}
	if f.srv.cookieName() == other.cookieName() {
		t.Fatal("two ports share a cookie name")
	}
}

// TestRespond_NotifySurvivesADroppedRequest: the browser going away after a
// submit must not cancel the notify.
func TestRespond_NotifySurvivesADroppedRequest(t *testing.T) {
	f := newFixture(t)
	f.login()
	ctx, cancel := context.WithCancel(t.Context())
	form := url.Values{"csrf": {f.csrf()}, "ask": {f.askID}, "kind": {"done"}}
	req := httptest.NewRequestWithContext(ctx, "POST", "/respond", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Host = testHost
	req.RemoteAddr = "127.0.0.1:50000"
	req.AddCookie(f.cookie)
	cancel()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	if len(f.notify.ctxErrs) != 1 || f.notify.ctxErrs[0] != nil {
		t.Fatalf("notify ran with a canceled context: %v", f.notify.ctxErrs)
	}
}

// TestRender_LinkHostsBesideLabels: an agent cannot label one site as
// another, in the ask's links or in its markdown steps.
func TestRender_LinkHostsBesideLabels(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Add(ask.Ask{Title: "Sign in", Bucket: ask.BucketNow,
		Steps: "Open [GitHub settings](https://evil.example/login) and <https://plain.example/x>",
		Links: []ask.Link{{Label: "GitHub settings", URL: "https://evil.example/login"}}}); err != nil {
		t.Fatal(err)
	}
	f.login()
	body := f.do("GET", "/", nil, nil, true).Body.String()
	for _, want := range []string{
		`GitHub settings</a> <span class="host">evil.example</span>`,
		`GitHub settings</a> (evil.example)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "plain.example/x</a> (plain.example)") {
		t.Error("a bare URL link got a redundant host")
	}
}
