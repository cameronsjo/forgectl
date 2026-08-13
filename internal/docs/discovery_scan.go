package docs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"sort"
	"sync"
)

// DiscoverServerInfo picks the docs server `forgectl docs open` should steer to.
//
// dir is the generation-owned record directory; legacyPath is the single
// pre-generation record, consulted only when no v1 record describes a server
// that answered. A missing directory is not a failure — it is the state before
// the first new server ever ran.
func DiscoverServerInfo(ctx context.Context, dir, legacyPath string) (DiscoveredServer, error) {
	return discoverServerInfo(ctx, productionDiscoveryRuntime(), dir, legacyPath)
}

func discoverServerInfo(ctx context.Context, rt discoveryRuntime, dir, legacyPath string) (DiscoveredServer, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	records, err := scanRecords(rt, dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No new-format directory yet. Legacy is the whole answer.
	case err != nil:
		// An unsafe directory or an overloaded one fails CLOSED. Falling back
		// to legacy here would let anyone who can make the v1 scan fail choose
		// which record `docs open` reads, which inverts the point of the scan.
		return DiscoveredServer{}, err
	default:
		info, err := selectLiveRecord(ctx, rt, records)
		if err == nil {
			return DiscoveredServer{Info: info}, nil
		}
		if !errors.Is(err, ErrNoServer) {
			return DiscoveredServer{}, err
		}
	}
	return discoverLegacyServer(ctx, rt, legacyPath)
}

// scanRecords enumerates, validates, and ranks the v1 records.
//
// Both caps are detected at cap+1 rather than by truncating at cap. Truncating
// would hand the choice of which servers are considered to filesystem
// enumeration order — and the subset it dropped could be the only live one.
func scanRecords(rt discoveryRuntime, dir string) ([]ServerInfo, error) {
	dirHandle, err := rt.openDir(dir, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("open the docs discovery directory: %w", sanitizeFSError(err))
	}
	defer dirHandle.Close() //nolint:errcheck // read-only scan; nothing to flush

	entries, err := dirHandle.ReadDir(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read the docs discovery directory: %w", sanitizeFSError(err))
	}
	if len(entries) > maxDirEntries {
		return nil, ErrDiscoveryOverloaded
	}

	// Memoize the local-IP check for the life of this scan. It enumerates
	// every interface on the machine, and without this a directory holding the
	// maximum 64 records would sweep them 64 times to answer the same handful
	// of questions. Per-scan rather than package-level, so an interface coming
	// up or down is picked up by the next discovery.
	rt.localIP = memoizeLocalIP(rt.localIP)

	records := make([]ServerInfo, 0, len(entries))
	for _, entry := range entries {
		generation, ok := generationFromFileName(entry.Name())
		if !ok {
			continue
		}
		info, err := readRecord(rt, dirHandle, entry.Name(), generation)
		if err != nil {
			// Unsafe, malformed, and future-version records are skipped, not
			// surfaced: one of them must never be able to hide a live sibling.
			continue
		}
		if len(records) == maxValidRecords {
			return nil, ErrDiscoveryOverloaded
		}
		records = append(records, info)
	}

	// Descending (started_at, generation). The generation tie-break is what
	// makes the order total: without it, two servers that started inside the
	// same nanosecond would rank by enumeration order, and two readers could
	// steer to different servers from identical state.
	sort.Slice(records, func(i, j int) bool {
		if !records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].StartedAt.After(records[j].StartedAt)
		}
		return records[i].Generation > records[j].Generation
	})
	return records, nil
}

// readRecord opens one candidate through the pinned directory, reads it under
// the size cap, and parses it strictly.
func readRecord(rt discoveryRuntime, dir discoveryDir, name, generation string) (ServerInfo, error) {
	file, err := dir.OpenRecord(name)
	if err != nil {
		return ServerInfo{}, err
	}
	defer file.Close() //nolint:errcheck // read-only

	// cap+1 so an oversized record is detected rather than silently truncated
	// into something that happens to parse.
	raw, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return ServerInfo{}, err
	}
	if len(raw) > maxRecordBytes {
		return ServerInfo{}, errRecordTooLarge
	}

	info, err := parseRecord(raw, rt.localIP)
	if err != nil {
		return ServerInfo{}, err
	}
	// The filename and the payload must agree. They are the same claim written
	// twice, and a record whose halves disagree is one whose name says nothing
	// about what it contains — which is the property the whole lease scheme
	// rests on.
	if info.Generation != generation {
		return ServerInfo{}, errGenerationMismatch
	}
	return info, nil
}

// selectLiveRecord returns the highest-ranked record whose server answered its
// freshness probe.
//
// Selection walks ranks IN ORDER on per-rank channels, so probe completion
// timing cannot change the winner: a fast lower-ranked server never gets to be
// selected before a slower higher-ranked one has been decided. That determinism
// is the difference between "discovery prefers the newest server" and
// "discovery prefers whichever server happened to answer first".
func selectLiveRecord(ctx context.Context, rt discoveryRuntime, records []ServerInfo) (ServerInfo, error) {
	if len(records) == 0 {
		return ServerInfo{}, ErrNoServer
	}

	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	// One closure, not two defers. Deferred calls run LIFO, so `defer cancel()`
	// followed by `defer workers.Wait()` would wait BEFORE cancelling — and the
	// workers only unblock on cancellation, so it would deadlock.
	defer func() {
		cancel()
		workers.Wait()
	}()

	done := make([]chan error, len(records))
	for i := range done {
		done[i] = make(chan error, 1)
	}
	slots := make(chan struct{}, maxConcurrentProbes)

	for i, record := range records {
		workers.Add(1)
		go func(rank int, rec ServerInfo) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				done[rank] <- ctx.Err()
				return
			}
			defer func() { <-slots }()

			probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
			defer probeCancel()
			done[rank] <- rt.http.ProbeGeneration(probeCtx, rec.Addr, rec.Generation)
		}(i, record)
	}

	for rank := range records {
		select {
		case err := <-done[rank]:
			if err == nil {
				return records[rank], nil
			}
		case <-ctx.Done():
			// The caller's deadline expired before every rank above a possible
			// winner was known. Returning a lower-ranked live server here would
			// make the answer depend on timing, which is the thing this
			// function exists to prevent.
			return ServerInfo{}, ctx.Err()
		}
	}
	return ServerInfo{}, ErrNoServer
}

// memoizeLocalIP caches one local-IP verdict per address for the life of a
// single scan. It is not safe for concurrent use and does not need to be: the
// scan that installs it is sequential, and selection has already finished
// parsing by the time it fans out probes.
func memoizeLocalIP(localIP func(netip.Addr) bool) func(netip.Addr) bool {
	if localIP == nil {
		return nil
	}
	seen := make(map[netip.Addr]bool)
	return func(ip netip.Addr) bool {
		if verdict, ok := seen[ip]; ok {
			return verdict
		}
		verdict := localIP(ip)
		seen[ip] = verdict
		return verdict
	}
}
