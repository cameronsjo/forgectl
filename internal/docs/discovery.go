package docs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// dialTimeout bounds the liveness probe in ReadServerInfo. Short because the
// target is always a loopback address — anything slower than this is not a
// server the caller wants to wait on.
const dialTimeout = 500 * time.Millisecond

// locateTimeout bounds the locate request. Also loopback-local, but more
// generous than dialTimeout because the server may be mid-index-rebuild.
const locateTimeout = 5 * time.Second

// ServerInfo is what a running `docs serve` publishes so another forgectl
// invocation can find and steer it. Written after the listener's address
// resolves (the default bind is port 0, so the port is not knowable before
// then) and removed on shutdown.
type ServerInfo struct {
	// Addr is the resolved host:port the server bound, e.g. "127.0.0.1:3590".
	Addr string `json:"addr"`
	// Token is the bearer token required on every request, or empty when the
	// server runs without auth (the loopback-only default).
	Token string `json:"token,omitempty"`
	// PID lets a reader distinguish a live server from a discovery file left
	// behind by one that was killed without running its shutdown path.
	PID int `json:"pid"`
}

// ErrNoServer indicates no reachable docs server was found — either no
// discovery file exists, or the one on disk describes a server that is gone.
var ErrNoServer = errors.New("no running docs server found")

// WriteServerInfo publishes info atomically at path.
//
// The write goes to a temp file in the same directory and is then renamed over
// the target. A plain create-and-write would leave a window where a concurrent
// `docs open` reads a truncated file and fails to parse it, which would surface
// as "no running docs server" while a server is in fact running — a confusing
// failure for a race that rename eliminates outright.
//
// The file carries a bearer token when one is in use, so it is created 0600.
func WriteServerInfo(path string, info ServerInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	payload, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("encode server info: %w", err)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".docs-server-*.json")
	if err != nil {
		return fmt.Errorf("create temp server info: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op once the rename below succeeds

	// Chmod before writing the token, not after, so the secret is never briefly
	// world-readable.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck // already failing; the deferred Remove cleans up
		return fmt.Errorf("secure temp server info: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("write temp server info: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp server info: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install server info: %w", err)
	}
	return nil
}

// RemoveServerInfo deletes the discovery file. A missing file is not an error —
// shutdown should be idempotent.
func RemoveServerInfo(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReadServerInfo loads the discovery file and confirms the server it describes
// is actually reachable, returning ErrNoServer when it is not.
//
// Liveness is checked by DIALING the address, not by signalling the PID. A
// stale file can name a PID that has since been recycled by an unrelated
// process, so a PID check can report a live docs server that is really some
// other program — and it cannot detect a server that is running but wedged.
// Dialling tests the only property the caller actually needs.
func ReadServerInfo(path string) (ServerInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ServerInfo{}, ErrNoServer
		}
		return ServerInfo{}, fmt.Errorf("read server info: %w", err)
	}

	var info ServerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return ServerInfo{}, fmt.Errorf("parse server info %s: %w", path, err)
	}
	if info.Addr == "" {
		return ServerInfo{}, fmt.Errorf("server info %s: missing addr", path)
	}
	if !dialable(info.Addr) {
		return ServerInfo{}, ErrNoServer
	}
	return info, nil
}

// dialable reports whether something is listening at addr right now.
func dialable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return false
	}
	conn.Close() //nolint:errcheck // liveness probe only
	return true
}

// DocURL builds the reader URL for a (root, relPath) pair on this server.
// relPath segments are escaped individually so a space or a '#' in a filename
// cannot truncate or reshape the path.
func (info ServerInfo) DocURL(rootLabel, relPath string) string {
	u := url.URL{
		Scheme: "http",
		Host:   info.Addr,
		Path:   "/doc/" + rootLabel + "/" + relPath,
	}
	return u.String()
}

// BaseURL is the reader's index page.
func (info ServerInfo) BaseURL() string {
	return (&url.URL{Scheme: "http", Host: info.Addr, Path: "/"}).String()
}

// ErrNotServed indicates the running reader does not serve the requested path —
// it is outside every configured root, under an excluded directory, or not a
// markdown file.
var ErrNotServed = errors.New("path is not served by the running docs reader")

// LocateDoc asks the running server which (root label, relative path) pair
// serves absPath.
//
// The question goes to the SERVER rather than being computed here on purpose:
// the server's index owns root labels, the walk's directory exclusions, and the
// single-file-root restriction, and it reflects whatever live reload has most
// recently rebuilt. Duplicating that logic client-side would produce URLs for
// files the server then refuses to serve, and would drift the first time either
// half changed.
func LocateDoc(info ServerInfo, absPath string) (root string, rel string, err error) {
	endpoint := url.URL{
		Scheme:   "http",
		Host:     info.Addr,
		Path:     locatePath,
		RawQuery: url.Values{"path": []string{absPath}}.Encode(),
	}

	req, err := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", err
	}
	// The Host header must be one the server's allowlist accepts; using the
	// bound address satisfies that for both the loopback and the exposed case.
	if info.Token != "" {
		req.Header.Set("Authorization", "Bearer "+info.Token)
	}

	client := &http.Client{Timeout: locateTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("ask the running reader about %s: %w", absPath, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response fully consumed below

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", "", fmt.Errorf("%w: %s", ErrNotServed, absPath)
	case http.StatusUnauthorized:
		return "", "", fmt.Errorf("the running reader rejected the stored token; restart `forgectl docs serve`")
	default:
		return "", "", fmt.Errorf("locate %s: reader returned %s", absPath, resp.Status)
	}

	var payload locateResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", fmt.Errorf("decode locate response: %w", err)
	}
	if payload.Root == "" || payload.Rel == "" {
		return "", "", fmt.Errorf("%w: %s", ErrNotServed, absPath)
	}
	return payload.Root, payload.Rel, nil
}
