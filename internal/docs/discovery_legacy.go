package docs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/netip"
)

// ErrLegacyProtectedServer means the discovered server predates the freshness
// endpoint AND requires a bearer token, so there is no safe way to steer it.
//
// The stored token is deliberately not sent. Steering means opening a
// connection to an address a record named and presenting a credential to
// whatever answers — and against a legacy server there is no way to first
// establish that the listener is the one the record described. The freshness
// probe is exactly the check that closes that gap, and an old server cannot
// answer it. Restarting the server is the fix, and it is a cheap one.
var ErrLegacyProtectedServer = errors.New("the running docs server predates generation-owned discovery and requires a bearer token; restart it with `forgectl docs serve`")

// legacyServerInfo is the pre-generation record: an address, an optional token,
// and a pid that was never load-bearing. pid is decoded and ignored rather than
// rejected, because rejecting it would make every record an old server wrote
// unreadable.
type legacyServerInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token,omitempty"`
	PID   int    `json:"pid,omitempty"`
}

// discoverLegacyServer is the fallback consulted only after the v1 scan
// completed safely and found no live server.
//
// Liveness here is a bounded DIAL, not a probe. There is no endpoint to ask,
// so the only question an old server can answer is whether anything is
// listening — which is weaker evidence than a generation match, and the reason
// a protected legacy server is never handed a token.
func discoverLegacyServer(ctx context.Context, rt discoveryRuntime, legacyPath string) (DiscoveredServer, error) {
	if legacyPath == "" {
		return DiscoveredServer{}, ErrNoServer
	}

	info, err := readLegacyRecord(rt, legacyPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DiscoveredServer{}, ErrNoServer
		}
		return DiscoveredServer{}, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := rt.http.Dial(dialCtx, info.Addr); err != nil {
		return DiscoveredServer{}, ErrNoServer
	}
	return DiscoveredServer{Info: info, Legacy: true}, nil
}

// readLegacyRecord opens the old single-record file through the platform's
// safe-open path and parses it under the same bounds as a v1 record.
//
// It never deletes or rewrites the file. An old binary rolled back onto this
// machine has to find its own record intact, and a new server automating that
// removal would recreate exactly the cross-process deletion forgectl#277 is
// about — just aimed at a different pathname.
func readLegacyRecord(rt discoveryRuntime, path string) (ServerInfo, error) {
	file, err := openLegacyRecord(path)
	if err != nil {
		return ServerInfo{}, err
	}
	defer file.Close() //nolint:errcheck // read-only

	raw, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return ServerInfo{}, sanitizeFSError(err)
	}
	if len(raw) > maxRecordBytes {
		return ServerInfo{}, errRecordTooLarge
	}

	var legacy legacyServerInfo
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return ServerInfo{}, errMalformedRecord
	}
	if legacy.Token != "" && !validDiscoveryToken(legacy.Token) {
		return ServerInfo{}, errInvalidToken
	}
	if err := validateLegacyAddr(legacy.Addr, rt.localIP); err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{Addr: legacy.Addr, Token: legacy.Token}, nil
}

// validateLegacyAddr applies the v1 address rules to an old record. The old
// writer emitted whatever net.Listener.Addr() produced, which is already the
// numeric form these rules accept.
func validateLegacyAddr(addr string, localIP func(netip.Addr) bool) error {
	return validateAdvertisedAddr(addr, localIP)
}
