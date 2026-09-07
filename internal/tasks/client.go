package tasks

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// DefaultHost is tasks.sjo.lol's own hostname — the one instance this client
// is written against (Vikunja v2.5.0). Configurable so a caller can point at
// a different instance without a code change.
const DefaultHost = "tasks.sjo.lol"

// pageSize is this instance's measured max_items_per_page. A caller cannot
// ask for more; asking for less just means more pages.
const pageSize = 50

// requestTimeout bounds every single HTTP call this client makes. There is
// no `timeout` binary to reach for in Go — a context deadline is the whole
// mechanism.
const requestTimeout = 10 * time.Second

// Client is a read-only Vikunja API client. It never mutates: only GET
// requests are ever issued, and no method here can be reached through any
// other verb.
type Client struct {
	baseURL    string
	token      Token
	httpClient *http.Client
}

// NewClient builds a Client for host, after refusing to proceed if host
// resolves anywhere the bearer token must not go (see checkHostPinning).
// token is read once by the caller via ReadToken and handed in already
// validated; NewClient does not read the keychain itself, so a caller can
// unit-test client construction without one.
func NewClient(ctx context.Context, runner exec.Runner, host string, token Token) (*Client, error) {
	if !token.Present() {
		return nil, fmt.Errorf("tasks: no token supplied")
	}
	vetted, gateway, err := checkHostPinning(ctx, runner, host)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: "https://" + host + "/api/v1",
		token:   token,
		httpClient: &http.Client{
			// A read-only API client has no reason to follow a redirect,
			// and following one is a way to lose the credential: Go strips
			// Authorization only when the redirect leaves the original
			// host, and that comparison is on host:port and ignores the
			// SCHEME — so https -> http on the same canonical address keeps
			// the header and carries the bearer token in cleartext over the
			// pinned dial. Refusing outright costs nothing here.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				// The vetted addresses are dialed directly. Without this the
				// transport would resolve `host` a SECOND time, independently
				// of the pin — and a hostile resolver answering the two
				// lookups differently is precisely the adversary the pin
				// exists for. TLS still verifies the certificate against
				// `host` (the URL is unchanged), so substituting the address
				// weakens nothing: a wrong address now fails the handshake
				// instead of receiving the bearer token.
				DialContext: pinnedDialer(vetted, gateway),
			},
		},
	}, nil
}

// NewClientForTesting builds a Client against an arbitrary base URL (an
// httptest server) with no host pinning and no TLS requirement. Exported
// for cross-package tests (internal/cli's command-level integration tests)
// that substitute this for NewClient via a test seam — see internal/cli's
// newTasksClient.
//
// It skips EVERY control NewClient installs: no host pinning, no TLS floor
// (a plain http:// base URL is accepted), no pinned dialer, no redirect
// refusal — and http.DefaultClient honours HTTP_PROXY/HTTPS_PROXY, so a
// token sent through it can egress via an ambient proxy. A doc comment is
// not a control, so the panic below is: it makes a non-test caller a
// startup failure rather than a silent, fully-unpinned credentialed client
// that no reviewer would spot from the call site.
func NewClientForTesting(baseURL string, token Token) *Client {
	if !testing.Testing() {
		panic("tasks.NewClientForTesting called outside a test — it skips host pinning, " +
			"the TLS floor, and the redirect refusal; use NewClient")
	}
	return &Client{baseURL: baseURL, token: token, httpClient: http.DefaultClient}
}

// get performs one bounded, redacting GET against path+query and returns the
// response body — but only after asserting the status code. Vikunja returns
// a JSON *object* on error where a caller expects an array, so the status is
// checked BEFORE any decode is attempted: a 401 body decoded as a task list
// would otherwise read as a confident empty result instead of a rejection.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("tasks: build request: %w", err)
	}
	req.Header.Set("Authorization", c.token.Header())
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A host-pin refusal from the dialer must escape with its sentinel
		// INTACT. Folding it into ErrUnreachable below would be worse than
		// losing the exit code: the caller serves stale cache on
		// ErrUnreachable and exits 0, so a refusal to send the credential
		// would present as a successful command. The verdict channel the
		// pin exists to feed would be dead, and quietly.
		if IsHostRefused(err) {
			return nil, err
		}
		// Everything else — dial failure, DNS failure, or the context
		// deadline above — surfaces as a *url.Error wrapping the real
		// cause. None is an auth verdict: the server was never reached.
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s -> %d", ErrUnauthorized, path, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("%w: %s -> %d", ErrUnexpectedStatus, path, resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUnreachable, readErr)
	}
	return body, nil
}

// maxPages bounds fetchAllPages. Termination is otherwise decided entirely
// by the server: a host that returns a full page forever — misbehaving,
// misconfigured, or hostile — spins this loop and grows the result slice
// without limit. At pageSize 50 this ceiling is 10,000 items, orders of
// magnitude past any real board, so it can only ever fire on a server that
// is lying about its own pagination.
const maxPages = 200

// maxItems bounds the ACCUMULATED result across pages. maxPages alone bounds
// iterations, not memory: each body is capped at 8 MiB by io.LimitReader, so
// 200 pages of maximally-large responses is on the order of a gigabyte of
// decoded objects before the page cap trips. The page comment reasons in
// items and so reads tighter than it is; this is the bound that matches it.
const maxItems = maxPages * pageSize

// getPage fetches one page of a paginated GET, decoding it exactly once into
// T, and reports whether the page was full (meaning a next page may exist).
func getPage[T any](ctx context.Context, c *Client, path string, page int) (items []T, full bool, err error) {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(pageSize))
	body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, false, fmt.Errorf("%w: %s: decode page %d: %v", ErrUnexpectedStatus, path, page, err)
	}
	return items, len(items) == pageSize, nil
}

// fetchAllPages walks path's pagination to exhaustion, honoring the
// instance's rate limit by making calls strictly sequentially (never
// concurrent) — 100 req/60s is comfortably above what a single operator's
// project/task/label counts require, but nothing here bursts regardless.
func fetchAllPages[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T
	for page := 1; page <= maxPages; page++ {
		items, full, err := getPage[T](ctx, c, path, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(out) > maxItems {
			return nil, fmt.Errorf("%w: %s: returned more than %d items — refusing to keep accumulating",
				ErrUnexpectedStatus, path, maxItems)
		}
		if !full {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: %s: still returning full pages after %d pages — the server is not paginating correctly",
		ErrUnexpectedStatus, path, maxPages)
}

// FetchProjects returns every project.
func (c *Client) FetchProjects(ctx context.Context) ([]Project, error) {
	return fetchAllPages[Project](ctx, c, "/projects")
}

// FetchTasks returns every task, including each one's related_tasks map.
func (c *Client) FetchTasks(ctx context.Context) ([]Task, error) {
	return fetchAllPages[Task](ctx, c, "/tasks")
}

// FetchLabels returns every label.
func (c *Client) FetchLabels(ctx context.Context) ([]Label, error) {
	return fetchAllPages[Label](ctx, c, "/labels")
}

// FetchAll builds a full Snapshot: projects, tasks (with relations), and
// labels. Sequential, not concurrent — see fetchAllPages.
func (c *Client) FetchAll(ctx context.Context) (Snapshot, error) {
	projects, err := c.FetchProjects(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	tasksList, err := c.FetchTasks(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	labels, err := c.FetchLabels(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Projects: projects, Tasks: tasksList, Labels: labels}, nil
}

// IsUnreachable reports whether err (or anything it wraps) is the
// unreachable sentinel — the network-failure case a caller may serve stale
// cache data for, with the cache's age stated.
func IsUnreachable(err error) bool { return errors.Is(err, ErrUnreachable) }

// IsUnauthorized reports whether err (or anything it wraps) is the
// unauthorized sentinel — the case a caller must NEVER paper over with a
// cache fallback.
func IsUnauthorized(err error) bool { return errors.Is(err, ErrUnauthorized) }
