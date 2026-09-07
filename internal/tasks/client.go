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
	if err := checkHostPinning(ctx, runner, host); err != nil {
		return nil, err
	}
	return &Client{
		baseURL: "https://" + host + "/api/v1",
		token:   token,
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

// NewClientForTesting builds a Client against an arbitrary base URL (an
// httptest server) with no host pinning and no TLS requirement. Exported
// for cross-package tests (internal/cli's command-level integration tests)
// that substitute this for NewClient via a test seam — see internal/cli's
// newTasksClient. Not for production use: it skips the pinning check
// NewClient exists to enforce.
func NewClientForTesting(baseURL string, token Token) *Client {
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
		// A dial failure, DNS failure, or the context deadline above all
		// surface here as a *url.Error wrapping the real cause. None of
		// them are an auth verdict — the server was never reached.
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

// getPage fetches one page of a paginated GET and reports whether the page
// was full (meaning a next page may exist).
func (c *Client) getPage(ctx context.Context, path string, page int) ([]byte, bool, error) {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(pageSize))
	body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, false, err
	}
	var probe []json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false, fmt.Errorf("%w: %s: decode page %d: %v", ErrUnexpectedStatus, path, page, err)
	}
	return body, len(probe) == pageSize, nil
}

// fetchAllPages walks path's pagination to exhaustion, honoring the
// instance's rate limit by making calls strictly sequentially (never
// concurrent) — 100 req/60s is comfortably above what a single operator's
// project/task/label counts require, but nothing here bursts regardless.
func fetchAllPages[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T
	for page := 1; ; page++ {
		body, full, err := c.getPage(ctx, path, page)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("%w: %s: decode page %d: %v", ErrUnexpectedStatus, path, page, err)
		}
		out = append(out, items...)
		if !full {
			return out, nil
		}
	}
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
