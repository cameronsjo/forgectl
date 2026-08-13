package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// discoveryIdentityPath answers exactly one question, before authentication:
// "which generation is this listener serving?"
const discoveryIdentityPath = "/.well-known/forgectl-docs"

// generationHeader carries the answer. It is a non-secret value: knowing it
// proves nothing and grants nothing.
const generationHeader = "X-Forgectl-Docs-Generation"

// maxResponseHeaderBytes bounds what the discovery client will read before a
// response body starts, so a hostile listener cannot stream headers forever.
const maxResponseHeaderBytes = 8 << 10

// Fixed client failures. Every one of them is coarse on purpose: the address,
// the generation, the redirect target, and the response body all come from a
// listener the reader has not yet established anything about, and none of them
// belongs on the operator's terminal.
var (
	errProbeUnreachable = errors.New("the docs server did not answer its discovery probe")
	errProbeMismatch    = errors.New("the listener at that address is not the discovered docs server")
	errProbeRedirect    = errors.New("the docs discovery probe was redirected")
	errLocateRedirect   = errors.New("the docs locate request was redirected")
	errLocateOversize   = errors.New("the docs server returned an oversized locate response")
	errStaleGeneration  = errors.New("the docs server at that address is no longer the one discovered; restart `forgectl docs serve`")
)

// DiscoveryIdentity serves the pre-auth freshness endpoint.
//
// It is middleware rather than a mux route because it has to sit at a specific
// point in the chain: behind the Host allowlist and the cross-site gate, so a
// hostile page cannot reach it through a victim's browser, but AHEAD of bearer
// authentication, because a reader asking "are you the server I found?" has by
// definition not decided yet whether to present a credential.
//
// What the endpoint proves is bounded and worth stating: the listener knew a
// random 128-bit value at probe time. It is freshness evidence against
// accidental stale-port reuse, not authentication. The value is non-secret and
// replayable, and the listener can change between the probe and the next
// request.
//
// currentGeneration is a load-only closure over the server's own generation
// source, so the endpoint always answers for whatever generation is live rather
// than for a value captured when the chain was built.
func DiscoveryIdentity(currentGeneration func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != discoveryIdentityPath {
				next.ServeHTTP(w, r)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set(generationHeader, currentGeneration())
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// ProbeServerGeneration asks the listener at addr whether it is serving
// generation, returning nil only when it answered exactly that.
//
// It takes addr and generation rather than a ServerInfo deliberately. A caller
// holding a ServerInfo holds a bearer token, and the single most damaging thing
// this probe could do is transmit one — the probe runs BEFORE the reader has
// any evidence about who is listening. Narrowing the signature makes that
// mistake unavailable rather than merely discouraged.
//
// This is the only probe implementation in the codebase. `internal/cli` calls
// it for its startup self-check instead of building a second client, so the
// no-proxy, no-redirect, no-credential contract has one definition.
func ProbeServerGeneration(ctx context.Context, addr, generation string) error {
	return discoveryHTTPClient{}.ProbeGeneration(ctx, addr, generation)
}

// discoveryHTTPClient is the production network surface: one dedicated client
// with every ambient behavior turned off.
type discoveryHTTPClient struct{}

// discoveryTransport deliberately does not use http.DefaultTransport.
//
// The default consults HTTP_PROXY/HTTPS_PROXY, and a proxy in this path would
// mean the bearer token for a loopback docs server leaves the machine. Every
// other setting here is a bound: a request to an address a record named must
// not be able to consume unbounded time or memory.
var discoveryTransport = &http.Transport{
	Proxy: nil,
	DialContext: (&net.Dialer{
		Timeout: dialTimeout,
	}).DialContext,
	DisableCompression:     true,
	DisableKeepAlives:      true,
	MaxResponseHeaderBytes: maxResponseHeaderBytes,
	ForceAttemptHTTP2:      false,
	TLSHandshakeTimeout:    dialTimeout,
}

// discoveryClient refuses redirects outright. Following one would let a
// listener redirect the reader — and, on the locate request, the Authorization
// header — to an address no record ever named and no probe ever checked.
var discoveryClient = &http.Client{
	Transport: discoveryTransport,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errProbeRedirect
	},
	Jar: nil,
}

func (discoveryHTTPClient) ProbeGeneration(ctx context.Context, addr, generation string) error {
	if !validGeneration(generation) {
		return errInvalidGeneration
	}
	if err := validateAdvertisedAddr(addr, isLocallyAssignedIP); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	endpoint := url.URL{Scheme: "http", Host: addr, Path: discoveryIdentityPath}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errProbeUnreachable
	}
	// No Authorization header, no cookies. Stated here because the absence is
	// the security property, and an absence leaves nothing for a reader to
	// notice later.
	resp, err := discoveryClient.Do(req)
	if err != nil {
		if errors.Is(err, errProbeRedirect) {
			return errProbeRedirect
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errProbeUnreachable
		}
		return errProbeUnreachable
	}
	defer resp.Body.Close() //nolint:errcheck // bounded read below

	if resp.StatusCode != http.StatusNoContent {
		return errProbeMismatch
	}
	if resp.Header.Get(generationHeader) != generation {
		return errProbeMismatch
	}
	// A 204 carrying a body is not the endpoint this code serves. Reading one
	// byte is enough to tell, and the LimitReader keeps a hostile listener from
	// turning the check into a download.
	extra, err := io.ReadAll(io.LimitReader(resp.Body, 1))
	if err != nil || len(extra) != 0 {
		return errProbeMismatch
	}
	return nil
}

// Dial is liveness for a LEGACY server, which has no freshness endpoint. It
// answers only "is anything listening there", which is why a legacy server that
// requires a token is never steered — see ErrLegacyProtectedServer.
func (discoveryHTTPClient) Dial(ctx context.Context, addr string) error {
	if err := validateAdvertisedAddr(addr, isLocallyAssignedIP); err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return errProbeUnreachable
	}
	conn.Close() //nolint:errcheck // liveness probe only
	return nil
}

// Locate asks the running server which (root label, relative path) pair serves
// absPath.
//
// The question goes to the SERVER rather than being computed client-side on
// purpose: the server's index owns root labels, the walk's directory
// exclusions, and the single-file-root restriction, and it reflects whatever
// live reload most recently rebuilt. Duplicating that here would produce URLs
// for files the server then refuses to serve.
func (discoveryHTTPClient) Locate(ctx context.Context, info ServerInfo, absPath string) (string, string, error) {
	if err := validateAdvertisedAddr(info.Addr, isLocallyAssignedIP); err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(ctx, locateTimeout)
	defer cancel()

	endpoint := url.URL{
		Scheme:   "http",
		Host:     info.Addr,
		Path:     locatePath,
		RawQuery: url.Values{"path": []string{absPath}}.Encode(),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", err
	}
	if info.Token != "" {
		req.Header.Set("Authorization", "Bearer "+info.Token)
	}

	resp, err := discoveryClient.Do(req)
	if err != nil {
		if errors.Is(err, errProbeRedirect) {
			return "", "", errLocateRedirect
		}
		return "", "", fmt.Errorf("ask the running reader about %s: %w", absPath, errProbeUnreachable)
	}
	defer resp.Body.Close() //nolint:errcheck // bounded read below

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", "", fmt.Errorf("%w: %s", ErrNotServed, absPath)
	case http.StatusUnauthorized:
		return "", "", errStaleGeneration
	default:
		return "", "", fmt.Errorf("locate %s: reader returned %s", absPath, resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRecordBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("read the locate response for %s: %w", absPath, errProbeUnreachable)
	}
	if len(raw) > maxRecordBytes {
		return "", "", errLocateOversize
	}

	var payload locateResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", fmt.Errorf("decode locate response: %w", err)
	}
	if payload.Root == "" || payload.Rel == "" {
		return "", "", fmt.Errorf("%w: %s", ErrNotServed, absPath)
	}
	return payload.Root, payload.Rel, nil
}

// LocateDoc resolves absPath on a discovered server.
//
// For a protected v1 server it re-runs the credential-free freshness probe
// IMMEDIATELY before the authenticated request. That narrows the window between
// "this address was the discovered server" and "this address receives the
// token" to about one round trip. It is deliberately not called atomic: a
// listener can still change in between, and generic LAN HTTP remains observable
// to a network peer either way.
func LocateDoc(ctx context.Context, server DiscoveredServer, absPath string) (root, rel string, err error) {
	return locateDoc(ctx, productionDiscoveryRuntime(), server, absPath)
}

func locateDoc(ctx context.Context, rt discoveryRuntime, server DiscoveredServer, absPath string) (string, string, error) {
	if server.Legacy {
		if server.Info.Token != "" {
			return "", "", ErrLegacyProtectedServer
		}
		return rt.http.Locate(ctx, server.Info, absPath)
	}
	if server.Info.Token != "" {
		if err := rt.http.ProbeGeneration(ctx, server.Info.Addr, server.Info.Generation); err != nil {
			return "", "", errStaleGeneration
		}
	}
	return rt.http.Locate(ctx, server.Info, absPath)
}
