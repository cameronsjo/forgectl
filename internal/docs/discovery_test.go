package docs

// Test plan for the generation-owned discovery store (forgectl#277).
//
// Filename grammar and schema (Classification: parser — hostile input)
//   [x] recordFileName / generationFromFileName round-trip
//   [x] uppercase hex, separators, traversal, controls, hidden temps, and
//       Unicode confusables are all refused as record names
//   [x] duplicate keys reject (encoding/json would keep the LAST one)
//   [x] unknown v1 fields, missing required fields, and trailing JSON reject
//   [x] a future schema_version is a SKIP, not a malformed record
//   [x] 1.0 and 1e0 are not schema version 1
//   [x] noncanonical times (offset form, lowercase z) reject
//   [x] present-but-empty token rejects; absent token is unprotected
//   [x] exactly maxRecordBytes is accepted, one byte more is refused
//
// Address contract (Classification: security gate — where a token gets sent)
//   [x] concrete IPv4/IPv6 listeners advertise their own numeric address
//   [x] unspecified listeners advertise same-family loopback
//   [x] port zero, non-TCP addresses, and IPv6 zones fail
//   [x] DNS names, unspecified, and remote addresses fail validation
//
// Publication and lease (Classification: cross-process ownership)
//   [x] publish then read back round-trips through a real directory
//   [x] a second publish of the same generation is ErrGenerationCollision and
//       does NOT overwrite the installed record
//   [x] closing one lease removes only that generation's record
//   [x] Close is idempotent and caches its result under concurrency
//   [x] a missing record at Close time is success
//   [x] injected create/chmod/short-write/sync/close/install failures leave no
//       final record
//   [x] a directory-sync failure yields a usable lease plus one warning
//
// Bounded scan and selection (Classification: DoS bounds + determinism)
//   [x] 256 entries scan, 257 is ErrDiscoveryOverloaded
//   [x] 64 valid records scan, 65 is ErrDiscoveryOverloaded
//   [x] malformed and unsafe entries do not mask a live in-cap record
//   [x] newest live wins; newest dead falls through to older live
//   [x] equal timestamps tie-break by generation, descending
//   [x] probe completion order cannot change the winner
//
// Legacy fallback (Classification: mixed-version behavior)
//   [x] live v1 beats a live legacy record
//   [x] unprotected live legacy is used when no v1 record answers
//   [x] a protected legacy server returns restart guidance and sends no token
//   [x] an unsafe or overloaded v1 directory does NOT fall back to legacy
//
// Filesystem safety controls, Unix (Classification: same-user tampering)
//   [x] a symlinked record is refused at open (see discovery_dir_unix_test.go)
//   [x] a FIFO wearing a record name is refused by the type check
//   [x] a hardlinked record is refused by the nlink check
//   [x] a group-readable record is refused by the mode check
//   [x] a group-readable or symlinked discovery directory is refused at open
//   [x] publishing into a 0755 directory writes no token to disk at all
//   [ ] foreign-owned records and directories — needs a second uid, not covered
//
// Sanitization (Classification: terminal safety)
//   [x] errors carry no path, generation, address, token, or record body

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testGeneration(seed byte) string {
	raw := make([]byte, generationBytes)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return hex.EncodeToString(raw)
}

// allowLocal is the localIP predicate used by tests that need a non-loopback
// address to validate without depending on the host's real interfaces.
func allowLocal(netip.Addr) bool { return true }

func denyLocal(netip.Addr) bool { return false }

func testRuntime(t *testing.T) discoveryRuntime {
	t.Helper()
	return discoveryRuntime{
		now:     func() time.Time { return time.Unix(1_700_000_000, 123456789).UTC() },
		random:  &countingReader{},
		openDir: openDiscoveryDir,
		localIP: allowLocal,
		http:    &fakeDiscoveryHTTP{live: map[string]string{}},
	}
}

// countingReader produces deterministic, distinct generations without pulling
// on the process entropy pool, so a test can name the generation it expects.
type countingReader struct {
	mu sync.Mutex
	n  byte
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	for i := range p {
		p[i] = r.n + byte(i)
	}
	return len(p), nil
}

// fakeDiscoveryHTTP answers probes from a fixed addr->generation map, with an
// optional per-address delay so a test can prove that completion ORDER does not
// influence selection.
type fakeDiscoveryHTTP struct {
	mu      sync.Mutex
	live    map[string]string
	delay   map[string]time.Duration
	dialOK  map[string]bool
	located map[string][2]string
	probes  []string
	locates []string
}

func (f *fakeDiscoveryHTTP) ProbeGeneration(ctx context.Context, addr, generation string) error {
	f.mu.Lock()
	delay := f.delay[addr]
	want, ok := f.live[addr]
	f.probes = append(f.probes, addr)
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !ok || want != generation {
		return errProbeMismatch
	}
	return nil
}

func (f *fakeDiscoveryHTTP) Dial(_ context.Context, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialOK[addr] {
		return nil
	}
	return errProbeUnreachable
}

func (f *fakeDiscoveryHTTP) Locate(_ context.Context, info ServerInfo, absPath string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locates = append(f.locates, info.Addr+"|"+info.Token+"|"+absPath)
	if got, ok := f.located[info.Addr]; ok {
		return got[0], got[1], nil
	}
	return "", "", ErrNotServed
}

func (f *fakeDiscoveryHTTP) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.probes)
}

// mustInfo builds a valid record for a fixed address and generation.
func mustInfo(t *testing.T, addr, generation string, startedAt time.Time, token string) ServerInfo {
	t.Helper()
	info := ServerInfo{
		SchemaVersion: docsSchemaVersion,
		Generation:    generation,
		StartedAt:     canonicalTime(startedAt),
		Addr:          addr,
		Token:         token,
	}
	if err := validateServerInfo(info, allowLocal); err != nil {
		t.Fatalf("test fixture is not a valid record: %v", err)
	}
	return info
}

// ---------------------------------------------------------------------------
// filename grammar
// ---------------------------------------------------------------------------

func TestRecordFileName_RoundTrips(t *testing.T) {
	generation := testGeneration(0x10)
	name := recordFileName(generation)
	got, ok := generationFromFileName(name)
	if !ok || got != generation {
		t.Fatalf("generationFromFileName(%q) = (%q, %v), want (%q, true)", name, got, ok, generation)
	}
}

func TestGenerationFromFileName_RejectsEverythingElse(t *testing.T) {
	valid := testGeneration(0x20)
	hostile := []struct {
		name string
		why  string
	}{
		{strings.ToUpper(valid) + ".json", "uppercase hex would give one generation two filenames"},
		{valid, "no .json suffix"},
		{valid + ".JSON", "uppercase suffix"},
		{valid[:31] + ".json", "31 hex characters"},
		{valid + "0.json", "33 hex characters"},
		{"../" + valid + ".json", "traversal"},
		{"." + valid + ".json", "hidden"},
		{".tmp-0011223344556677", "publisher temp"},
		{valid[:30] + "gg.json", "non-hex characters"},
		{valid[:31] + "а.json", "Cyrillic confusable"},
		{valid[:31] + "\n.json", "control character"},
		{"docs-server.json", "the legacy record"},
		{"", "empty"},
	}
	for _, tc := range hostile {
		if _, ok := generationFromFileName(tc.name); ok {
			t.Errorf("generationFromFileName(%q) accepted the name — %s", tc.name, tc.why)
		}
	}
}

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

func canonicalRecordJSON(generation, started, addr string, token *string) string {
	payload := map[string]any{
		"schema_version": 1,
		"generation":     generation,
		"started_at":     started,
		"addr":           addr,
	}
	if token != nil {
		payload["token"] = *token
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestParseRecord_Canonical_Accepted(t *testing.T) {
	generation := testGeneration(0x30)
	raw := canonicalRecordJSON(generation, "2026-08-13T12:34:56.123456789Z", "127.0.0.1:3590", nil)

	info, err := parseRecord([]byte(raw), allowLocal)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if info.Generation != generation || info.Addr != "127.0.0.1:3590" || info.Token != "" {
		t.Fatalf("parseRecord = %+v, want the canonical fixture", info)
	}
	if got := info.StartedAt.Format(time.RFC3339Nano); got != "2026-08-13T12:34:56.123456789Z" {
		t.Errorf("StartedAt = %q, want the canonical text preserved", got)
	}
}

func TestParseRecord_DuplicateKeys_Rejected(t *testing.T) {
	generation := testGeneration(0x31)
	// encoding/json keeps the LAST duplicate, so without the token-stream pass
	// this record would advertise 127.0.0.1:1 to anything reading it casually
	// and apply 127.0.0.1:2.
	raw := fmt.Sprintf(
		`{"schema_version":1,"generation":%q,"started_at":"2026-08-13T12:34:56Z","addr":"127.0.0.1:1","addr":"127.0.0.1:2"}`,
		generation)

	_, err := parseRecord([]byte(raw), allowLocal)
	if !errors.Is(err, errDuplicateKeys) {
		t.Fatalf("parseRecord with a duplicate addr: err = %v, want errDuplicateKeys", err)
	}
}

func TestParseRecord_UnknownSchemaVersion_IsSkipNotMalformed(t *testing.T) {
	generation := testGeneration(0x32)
	// A v2 record carries a field this build has never heard of. The strict
	// decoder would call that malformed; the right answer is "not my version".
	raw := fmt.Sprintf(
		`{"schema_version":2,"generation":%q,"started_at":"2026-08-13T12:34:56Z","addr":"127.0.0.1:1","future_field":true}`,
		generation)

	_, err := parseRecord([]byte(raw), allowLocal)
	if !errors.Is(err, errUnknownSchema) {
		t.Fatalf("parseRecord on a v2 record: err = %v, want errUnknownSchema", err)
	}
}

func TestParseRecord_NumericSchemaSpellings_Rejected(t *testing.T) {
	generation := testGeneration(0x33)
	for _, spelling := range []string{"1.0", "1e0", "1.00", "\"1\""} {
		raw := fmt.Sprintf(
			`{"schema_version":%s,"generation":%q,"started_at":"2026-08-13T12:34:56Z","addr":"127.0.0.1:1"}`,
			spelling, generation)
		if _, err := parseRecord([]byte(raw), allowLocal); err == nil {
			t.Errorf("parseRecord accepted schema_version %s — a typed struct would, and then two byte sequences would name one version", spelling)
		}
	}
}

func TestParseRecord_StructuralFailures(t *testing.T) {
	generation := testGeneration(0x34)
	base := `"generation":%q,"started_at":"2026-08-13T12:34:56Z","addr":"127.0.0.1:1"`

	tests := []struct {
		name string
		raw  string
	}{
		{"unknown v1 field", fmt.Sprintf(`{"schema_version":1,`+base+`,"pid":42}`, generation)},
		{"missing generation", `{"schema_version":1,"started_at":"2026-08-13T12:34:56Z","addr":"127.0.0.1:1"}`},
		{"missing addr", fmt.Sprintf(`{"schema_version":1,"generation":%q,"started_at":"2026-08-13T12:34:56Z"}`, generation)},
		{"missing started_at", fmt.Sprintf(`{"schema_version":1,"generation":%q,"addr":"127.0.0.1:1"}`, generation)},
		{"trailing value", fmt.Sprintf(`{"schema_version":1,`+base+`} {"schema_version":1}`, generation)},
		{"not an object", `[]`},
		{"empty", ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRecord([]byte(tc.raw), allowLocal); err == nil {
				t.Fatalf("parseRecord accepted %s", tc.name)
			}
		})
	}
}

func TestParseRecord_NoncanonicalTime_Rejected(t *testing.T) {
	generation := testGeneration(0x35)
	for _, spelling := range []string{
		"2026-08-13T12:34:56+00:00",
		"2026-08-13T14:34:56+02:00",
		"2026-08-13t12:34:56z",
		"2026-08-13T12:34:56.000Z",
		"0001-01-01T00:00:00Z",
	} {
		raw := canonicalRecordJSON(generation, spelling, "127.0.0.1:1", nil)
		if _, err := parseRecord([]byte(raw), allowLocal); !errors.Is(err, errInvalidStartedAt) {
			t.Errorf("parseRecord(%q): err = %v, want errInvalidStartedAt — one instant must have exactly one record text", spelling, err)
		}
	}
}

func TestParseRecord_TokenPresence(t *testing.T) {
	generation := testGeneration(0x36)
	empty := ""
	valid := "Az09-._~+/==="

	if _, err := parseRecord([]byte(canonicalRecordJSON(generation, "2026-08-13T12:34:56Z", "127.0.0.1:1", &empty)), allowLocal); !errors.Is(err, errInvalidToken) {
		t.Errorf(`present-but-empty token: err = %v, want errInvalidToken — "" is a claim of a credential that cannot authenticate`, err)
	}
	info, err := parseRecord([]byte(canonicalRecordJSON(generation, "2026-08-13T12:34:56Z", "127.0.0.1:1", &valid)), allowLocal)
	if err != nil || info.Token != valid {
		t.Errorf("valid b64token: (%q, %v), want it preserved", info.Token, err)
	}
	bad := "not a token"
	if _, err := parseRecord([]byte(canonicalRecordJSON(generation, "2026-08-13T12:34:56Z", "127.0.0.1:1", &bad)), allowLocal); !errors.Is(err, errInvalidToken) {
		t.Errorf("invalid token grammar: err = %v, want errInvalidToken", err)
	}
}

func TestParseRecord_AddressRules(t *testing.T) {
	generation := testGeneration(0x37)
	tests := []struct {
		name    string
		addr    string
		localIP func(netip.Addr) bool
	}{
		{"dns name", "localhost:3590", allowLocal},
		{"unspecified v4", "0.0.0.0:3590", allowLocal},
		{"unspecified v6", "[::]:3590", allowLocal},
		{"zero port", "127.0.0.1:0", allowLocal},
		{"no port", "127.0.0.1", allowLocal},
		{"remote address", "203.0.113.7:3590", denyLocal},
		{"4-in-6 spelling", "[::ffff:127.0.0.1]:3590", allowLocal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := canonicalRecordJSON(generation, "2026-08-13T12:34:56Z", tc.addr, nil)
			if _, err := parseRecord([]byte(raw), tc.localIP); err == nil {
				t.Fatalf("parseRecord accepted %s (%q)", tc.name, tc.addr)
			}
		})
	}
}

func TestParseRecord_SizeBoundary(t *testing.T) {
	generation := testGeneration(0x38)
	// Pad with legal inter-token whitespace so the record stays canonical
	// while its byte length is under the test's control.
	body := canonicalRecordJSON(generation, "2026-08-13T12:34:56Z", "127.0.0.1:1", nil)
	pad := maxRecordBytes - len(body)
	exact := body[:len(body)-1] + strings.Repeat(" ", pad) + "}"
	if len(exact) != maxRecordBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(exact), maxRecordBytes)
	}
	if _, err := parseRecord([]byte(exact), allowLocal); err != nil {
		t.Fatalf("parseRecord on exactly %d bytes: %v, want it accepted", maxRecordBytes, err)
	}
}

func TestParseRecord_InvalidUTF8_Rejected(t *testing.T) {
	raw := []byte(`{"schema_version":1,"generation":"` + testGeneration(0x39) + `","started_at":"2026-08-13T12:34:56Z","addr":"127.0.0.1:1","x":"` + "\xff\xfe" + `"}`)
	if _, err := parseRecord(raw, allowLocal); !errors.Is(err, errMalformedRecord) {
		t.Fatalf("parseRecord on invalid UTF-8: err = %v, want errMalformedRecord", err)
	}
}

// ---------------------------------------------------------------------------
// advertised address
// ---------------------------------------------------------------------------

func TestAdvertisedDocsAddr(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		want string
		fail bool
	}{
		{name: "concrete v4", addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3590}, want: "127.0.0.1:3590"},
		{name: "concrete v6", addr: &net.TCPAddr{IP: net.IPv6loopback, Port: 3590}, want: "[::1]:3590"},
		{name: "unspecified v4 becomes loopback", addr: &net.TCPAddr{IP: net.IPv4zero, Port: 3590}, want: "127.0.0.1:3590"},
		{name: "unspecified v6 becomes loopback", addr: &net.TCPAddr{IP: net.IPv6unspecified, Port: 3590}, want: "[::1]:3590"},
		{name: "bare port becomes loopback", addr: &net.TCPAddr{Port: 3590}, want: "127.0.0.1:3590"},
		{name: "zero port fails", addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, fail: true},
		{name: "scoped v6 fails", addr: &net.TCPAddr{IP: net.IPv6loopback, Port: 3590, Zone: "en0"}, fail: true},
		{name: "non-TCP fails", addr: &net.UnixAddr{Name: "/tmp/x", Net: "unix"}, fail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AdvertisedDocsAddr(tc.addr)
			if tc.fail {
				if err == nil {
					t.Fatalf("AdvertisedDocsAddr(%v) = %q, want an error", tc.addr, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("AdvertisedDocsAddr(%v) = (%q, %v), want %q", tc.addr, got, err, tc.want)
			}
		})
	}
}

func TestAdvertisedDocsAddr_RealListenerValidates(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test fixture

	advertised, err := AdvertisedDocsAddr(ln.Addr())
	if err != nil {
		t.Fatalf("AdvertisedDocsAddr: %v", err)
	}
	if err := validateAdvertisedAddr(advertised, isLocallyAssignedIP); err != nil {
		t.Fatalf("a real listener's advertised address failed validation: %v", err)
	}
}

// ---------------------------------------------------------------------------
// publication, collision, and the lease
// ---------------------------------------------------------------------------

func TestPublishServerInfo_RoundTripsThroughDiscovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)

	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0x40), time.Unix(1700, 0), "")
	publication, err := publishServerInfo(rt, dir, info)
	if err != nil {
		t.Fatalf("publishServerInfo: %v", err)
	}
	if publication.Lease == nil {
		t.Fatal("publishServerInfo returned no lease on success")
	}
	if publication.Warning != nil {
		t.Errorf("unexpected warning: %v", publication.Warning)
	}
	t.Cleanup(func() { publication.Lease.Close() }) //nolint:errcheck // best effort

	fake := rt.http.(*fakeDiscoveryHTTP)
	fake.live[info.Addr] = info.Generation

	got, err := discoverServerInfo(context.Background(), rt, dir, "")
	if err != nil {
		t.Fatalf("discoverServerInfo: %v", err)
	}
	if got.Legacy || got.Info.Addr != info.Addr || got.Info.Generation != info.Generation {
		t.Fatalf("discoverServerInfo = %+v, want the published v1 record", got)
	}
	if !got.Info.StartedAt.Equal(info.StartedAt) {
		t.Errorf("StartedAt round-trip: got %v, want %v — in-memory and on-disk records must sort identically", got.Info.StartedAt, info.StartedAt)
	}
}

func TestPublishServerInfo_RecordIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0x41), time.Unix(1700, 0), "s3cret-token")

	publication, err := publishServerInfo(rt, dir, info)
	if err != nil {
		t.Fatalf("publishServerInfo: %v", err)
	}
	defer publication.Lease.Close() //nolint:errcheck // best effort

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want %o", got, 0o700)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, recordFileName(info.Generation)))
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("record mode = %o, want %o — the record carries a bearer token", got, 0o600)
	}
}

func TestPublishServerInfo_Collision_DoesNotOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	generation := testGeneration(0x42)

	first := mustInfo(t, "127.0.0.1:1111", generation, time.Unix(1700, 0), "")
	firstPub, err := publishServerInfo(rt, dir, first)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	defer firstPub.Lease.Close() //nolint:errcheck // best effort

	second := mustInfo(t, "127.0.0.1:2222", generation, time.Unix(1800, 0), "")
	if _, err := publishServerInfo(rt, dir, second); !errors.Is(err, ErrGenerationCollision) {
		t.Fatalf("second publish of the same generation: err = %v, want ErrGenerationCollision", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, recordFileName(generation)))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !strings.Contains(string(raw), "127.0.0.1:1111") {
		t.Fatalf("the installed record was overwritten by the colliding publish: %s", raw)
	}

	// No temp residue: a refused install must clean up after itself.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries after a collision, want 1", len(entries))
	}
}

func TestServerLease_Close_RemovesOnlyItsOwnRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)

	a := mustInfo(t, "127.0.0.1:1111", testGeneration(0x50), time.Unix(1700, 0), "")
	b := mustInfo(t, "127.0.0.1:2222", testGeneration(0x60), time.Unix(1800, 0), "")

	pubA, err := publishServerInfo(rt, dir, a)
	if err != nil {
		t.Fatalf("publish A: %v", err)
	}
	pubB, err := publishServerInfo(rt, dir, b)
	if err != nil {
		t.Fatalf("publish B: %v", err)
	}

	if err := pubA.Lease.Close(); err != nil {
		t.Fatalf("close lease A: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, recordFileName(a.Generation))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("A's record survived A's lease close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, recordFileName(b.Generation))); err != nil {
		t.Fatalf("B's record was removed by A's lease close: %v — this is forgectl#277", err)
	}

	if err := pubB.Lease.Close(); err != nil {
		t.Fatalf("close lease B: %v", err)
	}
}

func TestServerLease_Close_IdempotentUnderConcurrency(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0x51), time.Unix(1700, 0), "")

	publication, err := publishServerInfo(rt, dir, info)
	if err != nil {
		t.Fatalf("publishServerInfo: %v", err)
	}

	results := make([]error, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = publication.Lease.Close()
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent Close #%d = %v, want nil", i, err)
		}
	}
}

func TestServerLease_Close_MissingRecordIsSuccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0x52), time.Unix(1700, 0), "")

	publication, err := publishServerInfo(rt, dir, info)
	if err != nil {
		t.Fatalf("publishServerInfo: %v", err)
	}
	// An operator cleaning up by hand is the ordinary way this happens.
	if err := os.Remove(filepath.Join(dir, recordFileName(info.Generation))); err != nil {
		t.Fatalf("remove record: %v", err)
	}
	if err := publication.Lease.Close(); err != nil {
		t.Errorf("Close with the record already gone = %v, want nil — shutdown must be idempotent", err)
	}
}

// ---------------------------------------------------------------------------
// injected filesystem failures
// ---------------------------------------------------------------------------

// failingDir wraps a real directory and fails exactly one operation, so a test
// can prove that each individual failure leaves no final record — without
// changing filesystem permissions to synthesize it.
type failingDir struct {
	discoveryDir
	failCreate  error
	failChmod   error
	shortWrite  bool
	failSync    error
	failClose   error
	failInstall error
	failDirSync error
}

func (d *failingDir) CreateTemp() (discoveryWriteFile, string, error) {
	if d.failCreate != nil {
		return nil, "", d.failCreate
	}
	file, name, err := d.discoveryDir.CreateTemp()
	if err != nil {
		return nil, "", err
	}
	return &failingFile{discoveryWriteFile: file, dir: d}, name, nil
}

func (d *failingDir) InstallNoReplace(tempName, finalName string) (error, error) {
	if d.failInstall != nil {
		return nil, d.failInstall
	}
	return d.discoveryDir.InstallNoReplace(tempName, finalName)
}

func (d *failingDir) Sync() error {
	if d.failDirSync != nil {
		return d.failDirSync
	}
	return d.discoveryDir.Sync()
}

type failingFile struct {
	discoveryWriteFile
	dir *failingDir
}

func (f *failingFile) Chmod(mode fs.FileMode) error {
	if f.dir.failChmod != nil {
		return f.dir.failChmod
	}
	return f.discoveryWriteFile.Chmod(mode)
}

func (f *failingFile) Write(p []byte) (int, error) {
	if f.dir.shortWrite {
		if len(p) == 0 {
			return 0, nil
		}
		return f.discoveryWriteFile.Write(p[:len(p)-1])
	}
	return f.discoveryWriteFile.Write(p)
}

func (f *failingFile) Sync() error {
	if f.dir.failSync != nil {
		return f.dir.failSync
	}
	return f.discoveryWriteFile.Sync()
}

func (f *failingFile) Close() error {
	if f.dir.failClose != nil {
		f.discoveryWriteFile.Close() //nolint:errcheck // releasing the descriptor before reporting the injected failure
		return f.dir.failClose
	}
	return f.discoveryWriteFile.Close()
}

func runtimeWithFailingDir(t *testing.T, configure func(*failingDir)) (discoveryRuntime, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	rt.openDir = func(path string, create bool) (discoveryDir, error) {
		real, err := openDiscoveryDir(path, create)
		if err != nil {
			return nil, err
		}
		wrapped := &failingDir{discoveryDir: real}
		configure(wrapped)
		return wrapped, nil
	}
	return rt, dir
}

func TestPublishServerInfo_WriteFailures_LeaveNoFinalRecord(t *testing.T) {
	injected := errors.New("injected")
	tests := []struct {
		name      string
		configure func(*failingDir)
	}{
		{"create", func(d *failingDir) { d.failCreate = injected }},
		{"chmod", func(d *failingDir) { d.failChmod = injected }},
		{"short write", func(d *failingDir) { d.shortWrite = true }},
		{"file sync", func(d *failingDir) { d.failSync = injected }},
		{"file close", func(d *failingDir) { d.failClose = injected }},
		{"install", func(d *failingDir) { d.failInstall = injected }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, dir := runtimeWithFailingDir(t, tc.configure)
			info := mustInfo(t, "127.0.0.1:3590", testGeneration(0x70), time.Unix(1700, 0), "")

			publication, err := publishServerInfo(rt, dir, info)
			if err == nil {
				t.Fatalf("publishServerInfo with a %s failure returned nil error and lease %v", tc.name, publication.Lease)
			}
			if publication.Lease != nil {
				t.Errorf("a failed publication returned a lease")
			}
			if _, statErr := os.Stat(filepath.Join(dir, recordFileName(info.Generation))); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a %s failure left a final record behind: %v", tc.name, statErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatalf("read dir: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("a %s failure left %d entries behind, want 0", tc.name, len(entries))
			}
		})
	}
}

func TestPublishServerInfo_DirectorySyncFailure_IsWarningNotError(t *testing.T) {
	injected := errors.New("injected dirsync")
	rt, dir := runtimeWithFailingDir(t, func(d *failingDir) { d.failDirSync = injected })
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0x71), time.Unix(1700, 0), "")

	publication, err := publishServerInfo(rt, dir, info)
	if err != nil {
		t.Fatalf("publishServerInfo: %v, want a usable publication — the record is already visible, so a durability failure cannot be reported as its absence", err)
	}
	if publication.Lease == nil {
		t.Fatal("no lease returned alongside the warning")
	}
	defer publication.Lease.Close() //nolint:errcheck // best effort
	if publication.Warning == nil {
		t.Fatal("no warning returned for the directory-sync failure")
	}
	if _, statErr := os.Stat(filepath.Join(dir, recordFileName(info.Generation))); statErr != nil {
		t.Errorf("the final record is missing after a directory-sync warning: %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// bounded scan
// ---------------------------------------------------------------------------

func writeRawRecord(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeValidRecord(t *testing.T, dir string, seed byte, port int, started time.Time) ServerInfo {
	t.Helper()
	generation := testGeneration(seed)
	info := mustInfo(t, fmt.Sprintf("127.0.0.1:%d", port), generation, started, "")
	payload, err := encodeRecord(info)
	if err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}
	writeRawRecord(t, dir, recordFileName(generation), string(payload))
	return info
}

func TestScanRecords_EntryCapBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entries   int
		overloads bool
	}{
		{"at the cap", maxDirEntries, false},
		{"one past the cap", maxDirEntries + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "docs-servers")
			// Fill with names the reader ignores, so the ENTRY cap is what is
			// under test rather than the valid-record cap.
			for i := 0; i < tc.entries; i++ {
				writeRawRecord(t, dir, fmt.Sprintf("ignored-%04d", i), "{}")
			}
			_, err := scanRecords(testRuntime(t), dir)
			if tc.overloads {
				if !errors.Is(err, ErrDiscoveryOverloaded) {
					t.Fatalf("scanRecords with %d entries: err = %v, want ErrDiscoveryOverloaded", tc.entries, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("scanRecords with %d entries: %v", tc.entries, err)
			}
		})
	}
}

func TestScanRecords_ValidRecordCapBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		records   int
		overloads bool
	}{
		{"at the cap", maxValidRecords, false},
		{"one past the cap", maxValidRecords + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "docs-servers")
			for i := 0; i < tc.records; i++ {
				writeValidRecord(t, dir, byte(i+1), 3000+i, time.Unix(int64(1700+i), 0))
			}
			records, err := scanRecords(testRuntime(t), dir)
			if tc.overloads {
				if !errors.Is(err, ErrDiscoveryOverloaded) {
					t.Fatalf("scanRecords with %d valid records: err = %v, want ErrDiscoveryOverloaded — truncating would let enumeration order pick the subset", tc.records, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("scanRecords with %d valid records: %v", tc.records, err)
			}
			if len(records) != tc.records {
				t.Fatalf("scanRecords returned %d records, want %d", len(records), tc.records)
			}
		})
	}
}

func TestScanRecords_MalformedEntriesDoNotMaskALiveRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	live := writeValidRecord(t, dir, 0x80, 3590, time.Unix(1700, 0))

	writeRawRecord(t, dir, recordFileName(testGeneration(0x81)), "{not json")
	writeRawRecord(t, dir, recordFileName(testGeneration(0x82)), `{"schema_version":9}`)
	writeRawRecord(t, dir, recordFileName(testGeneration(0x83)), strings.Repeat("x", maxRecordBytes+1))
	// A record whose payload names a different generation than its filename.
	mismatched := mustInfo(t, "127.0.0.1:4444", testGeneration(0x84), time.Unix(1900, 0), "")
	payload, err := encodeRecord(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	writeRawRecord(t, dir, recordFileName(testGeneration(0x85)), string(payload))
	writeRawRecord(t, dir, "not-a-record.txt", "junk")

	records, err := scanRecords(testRuntime(t), dir)
	if err != nil {
		t.Fatalf("scanRecords: %v", err)
	}
	if len(records) != 1 || records[0].Generation != live.Generation {
		t.Fatalf("scanRecords = %+v, want only the one live record — a malformed sibling must never hide it", records)
	}
}

func TestScanRecords_SortsDescendingByStartThenGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	older := writeValidRecord(t, dir, 0x90, 3001, time.Unix(1700, 0))
	newerLow := writeValidRecord(t, dir, 0x01, 3002, time.Unix(1800, 0))
	newerHigh := writeValidRecord(t, dir, 0xF0, 3003, time.Unix(1800, 0))

	records, err := scanRecords(testRuntime(t), dir)
	if err != nil {
		t.Fatalf("scanRecords: %v", err)
	}
	want := []string{newerHigh.Generation, newerLow.Generation, older.Generation}
	for i, generation := range want {
		if records[i].Generation != generation {
			t.Fatalf("rank %d = %q, want %q — order must be total so two readers agree from identical state", i, records[i].Generation, generation)
		}
	}
}

func TestScanRecords_MissingDirectory_IsNotExist(t *testing.T) {
	_, err := scanRecords(testRuntime(t), filepath.Join(t.TempDir(), "never-created"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("scanRecords on a missing directory: err = %v, want fs.ErrNotExist — that is the state before the first new server ran", err)
	}
}

// ---------------------------------------------------------------------------
// selection
// ---------------------------------------------------------------------------

func TestSelectLiveRecord_PrefersNewestLive(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)

	older := mustInfo(t, "127.0.0.1:3001", testGeneration(0xA0), time.Unix(1700, 0), "")
	newer := mustInfo(t, "127.0.0.1:3002", testGeneration(0xB0), time.Unix(1800, 0), "")
	fake.live[older.Addr] = older.Generation
	fake.live[newer.Addr] = newer.Generation

	got, err := selectLiveRecord(context.Background(), rt, []ServerInfo{newer, older})
	if err != nil || got.Generation != newer.Generation {
		t.Fatalf("selectLiveRecord = (%+v, %v), want the newest live record", got, err)
	}
}

func TestSelectLiveRecord_NewestDeadFallsThroughToOlderLive(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)

	older := mustInfo(t, "127.0.0.1:3001", testGeneration(0xA1), time.Unix(1700, 0), "")
	newerDead := mustInfo(t, "127.0.0.1:3002", testGeneration(0xB1), time.Unix(1800, 0), "")
	fake.live[older.Addr] = older.Generation // newerDead answers nothing

	got, err := selectLiveRecord(context.Background(), rt, []ServerInfo{newerDead, older})
	if err != nil || got.Generation != older.Generation {
		t.Fatalf("selectLiveRecord = (%+v, %v), want the older live record — a dead sibling must not mask it", got, err)
	}
}

func TestSelectLiveRecord_CompletionOrderCannotChangeTheWinner(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	fake.delay = map[string]time.Duration{}

	// The highest-ranked server is the SLOWEST to answer. A selection that
	// took whichever probe finished first would pick the wrong one.
	winner := mustInfo(t, "127.0.0.1:3001", testGeneration(0xA2), time.Unix(1900, 0), "")
	fast := mustInfo(t, "127.0.0.1:3002", testGeneration(0xB2), time.Unix(1800, 0), "")
	fake.live[winner.Addr] = winner.Generation
	fake.live[fast.Addr] = fast.Generation
	fake.delay[winner.Addr] = 80 * time.Millisecond

	got, err := selectLiveRecord(context.Background(), rt, []ServerInfo{winner, fast})
	if err != nil || got.Generation != winner.Generation {
		t.Fatalf("selectLiveRecord = (%+v, %v), want the slow high-ranked record", got, err)
	}
}

func TestSelectLiveRecord_NoLiveRecords_ErrNoServer(t *testing.T) {
	rt := testRuntime(t)
	dead := mustInfo(t, "127.0.0.1:3001", testGeneration(0xA3), time.Unix(1700, 0), "")

	if _, err := selectLiveRecord(context.Background(), rt, []ServerInfo{dead}); !errors.Is(err, ErrNoServer) {
		t.Fatalf("selectLiveRecord with nothing live: err = %v, want ErrNoServer", err)
	}
}

func TestSelectLiveRecord_CancelledContext_DoesNotLeak(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	fake.delay = map[string]time.Duration{}

	records := make([]ServerInfo, 0, 16)
	for i := 0; i < 16; i++ {
		info := mustInfo(t, fmt.Sprintf("127.0.0.1:%d", 4000+i), testGeneration(byte(i+1)), time.Unix(int64(1700+i), 0), "")
		fake.delay[info.Addr] = time.Second
		records = append(records, info)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := selectLiveRecord(ctx, rt, records); err == nil {
			t.Error("selectLiveRecord on a cancelled context returned success")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// The natural `defer cancel(); defer wg.Wait()` deadlocks here: defers
		// run LIFO, so it would wait before cancelling and the workers only
		// unblock on cancellation.
		t.Fatal("selectLiveRecord did not return within 5s of a cancelled context")
	}
}

// ---------------------------------------------------------------------------
// legacy fallback
// ---------------------------------------------------------------------------

func writeLegacyRecord(t *testing.T, addr, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docs-server.json")
	payload := map[string]any{"addr": addr, "pid": 4242}
	if token != "" {
		payload["token"] = token
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverServerInfo_LiveV1BeatsLegacy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)

	v1 := writeValidRecord(t, dir, 0xC0, 3590, time.Unix(1700, 0))
	fake.live[v1.Addr] = v1.Generation

	legacyPath := writeLegacyRecord(t, "127.0.0.1:9999", "")
	fake.dialOK = map[string]bool{"127.0.0.1:9999": true}

	got, err := discoverServerInfo(context.Background(), rt, dir, legacyPath)
	if err != nil {
		t.Fatalf("discoverServerInfo: %v", err)
	}
	if got.Legacy || got.Info.Addr != v1.Addr {
		t.Fatalf("discoverServerInfo = %+v, want the live v1 record", got)
	}
}

func TestDiscoverServerInfo_DeadV1FallsBackToLiveLegacy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)

	writeValidRecord(t, dir, 0xC1, 3590, time.Unix(1700, 0)) // answers nothing
	legacyPath := writeLegacyRecord(t, "127.0.0.1:9999", "")
	fake.dialOK = map[string]bool{"127.0.0.1:9999": true}

	got, err := discoverServerInfo(context.Background(), rt, dir, legacyPath)
	if err != nil {
		t.Fatalf("discoverServerInfo: %v", err)
	}
	if !got.Legacy || got.Info.Addr != "127.0.0.1:9999" {
		t.Fatalf("discoverServerInfo = %+v, want the live legacy record", got)
	}
}

func TestDiscoverServerInfo_MissingV1DirectoryFallsBackToLegacy(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	legacyPath := writeLegacyRecord(t, "127.0.0.1:9999", "")
	fake.dialOK = map[string]bool{"127.0.0.1:9999": true}

	got, err := discoverServerInfo(context.Background(), rt, filepath.Join(t.TempDir(), "absent"), legacyPath)
	if err != nil {
		t.Fatalf("discoverServerInfo: %v", err)
	}
	if !got.Legacy {
		t.Fatalf("discoverServerInfo = %+v, want the legacy record", got)
	}
}

func TestDiscoverServerInfo_OverloadedV1_DoesNotBypassToLegacy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs-servers")
	for i := 0; i <= maxDirEntries; i++ {
		writeRawRecord(t, dir, fmt.Sprintf("ignored-%04d", i), "{}")
	}
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	legacyPath := writeLegacyRecord(t, "127.0.0.1:9999", "")
	fake.dialOK = map[string]bool{"127.0.0.1:9999": true}

	_, err := discoverServerInfo(context.Background(), rt, dir, legacyPath)
	if !errors.Is(err, ErrDiscoveryOverloaded) {
		t.Fatalf("discoverServerInfo on overloaded v1 state: err = %v, want ErrDiscoveryOverloaded — falling back would let anyone who can break the v1 scan choose the answer", err)
	}
}

func TestDiscoverServerInfo_NothingRunning_ErrNoServer(t *testing.T) {
	rt := testRuntime(t)
	_, err := discoverServerInfo(context.Background(), rt, filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("discoverServerInfo with nothing running: err = %v, want ErrNoServer", err)
	}
}

func TestLocateDoc_ProtectedLegacy_SendsNoTokenAndNamesRestart(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	server := DiscoveredServer{Info: ServerInfo{Addr: "127.0.0.1:9999", Token: "s3cret"}, Legacy: true}

	_, _, err := locateDoc(context.Background(), rt, server, "/abs/plan.md")
	if !errors.Is(err, ErrLegacyProtectedServer) {
		t.Fatalf("locateDoc against a protected legacy server: err = %v, want ErrLegacyProtectedServer", err)
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error = %q, want it to name the fix", err)
	}
	if len(fake.locates) != 0 {
		t.Fatalf("a protected legacy server received a locate request carrying its token: %v", fake.locates)
	}
}

func TestLocateDoc_ProtectedV1_ReprobesBeforeSendingTheToken(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0xD0), time.Unix(1700, 0), "s3cret")
	fake.live[info.Addr] = info.Generation
	fake.located = map[string][2]string{info.Addr: {"docs", "plan.md"}}

	root, rel, err := locateDoc(context.Background(), rt, DiscoveredServer{Info: info}, "/abs/plan.md")
	if err != nil || root != "docs" || rel != "plan.md" {
		t.Fatalf("locateDoc = (%q, %q, %v)", root, rel, err)
	}
	if fake.probeCount() != 1 {
		t.Fatalf("probes before the authenticated locate = %d, want 1", fake.probeCount())
	}
}

func TestLocateDoc_ProtectedV1_StaleGeneration_SendsNoToken(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0xD1), time.Unix(1700, 0), "s3cret")
	// Something else is listening on that port now.
	fake.live[info.Addr] = testGeneration(0xD2)

	if _, _, err := locateDoc(context.Background(), rt, DiscoveredServer{Info: info}, "/abs/plan.md"); !errors.Is(err, errStaleGeneration) {
		t.Fatalf("locateDoc against a replaced listener: err = %v, want the stale-generation refusal", err)
	}
	if len(fake.locates) != 0 {
		t.Fatalf("a replaced listener received the stored token: %v", fake.locates)
	}
}

func TestLocateDoc_UnprotectedV1_SkipsTheExtraProbe(t *testing.T) {
	rt := testRuntime(t)
	fake := rt.http.(*fakeDiscoveryHTTP)
	info := mustInfo(t, "127.0.0.1:3590", testGeneration(0xD3), time.Unix(1700, 0), "")
	fake.located = map[string][2]string{info.Addr: {"docs", "plan.md"}}

	if _, _, err := locateDoc(context.Background(), rt, DiscoveredServer{Info: info}, "/abs/plan.md"); err != nil {
		t.Fatalf("locateDoc: %v", err)
	}
	if fake.probeCount() != 0 {
		t.Errorf("probes = %d, want 0 — there is no credential to protect", fake.probeCount())
	}
}

// ---------------------------------------------------------------------------
// identity endpoint and the real client
// ---------------------------------------------------------------------------

func TestDiscoveryIdentity_AnswersOnlyItsOwnPath(t *testing.T) {
	generation := testGeneration(0xE0)
	handler := DiscoveryIdentity(func() string { return generation })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, discoveryIdentityPath, nil))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s %s: status = %d, want %d", method, discoveryIdentityPath, rec.Code, http.StatusNoContent)
		}
		if got := rec.Header().Get(generationHeader); got != generation {
			t.Errorf("%s: generation header = %q, want %q", method, got, generation)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", method, got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: body = %q, want empty", method, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, discoveryIdentityPath, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: status = %d, Allow = %q, want 405 and \"GET, HEAD\"", rec.Code, rec.Header().Get("Allow"))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("an unrelated path: status = %d, want it to fall through to the next handler", rec.Code)
	}
}

func TestProbeServerGeneration_AgainstARealListener(t *testing.T) {
	generation := testGeneration(0xE1)
	var sawAuthorization bool
	server := httptest.NewServer(DiscoveryIdentity(func() string { return generation })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				sawAuthorization = true
			}
			w.WriteHeader(http.StatusOK)
		})))
	defer server.Close()

	addr := server.Listener.Addr().String()
	if err := ProbeServerGeneration(context.Background(), addr, generation); err != nil {
		t.Fatalf("ProbeServerGeneration against its own listener: %v", err)
	}
	if sawAuthorization {
		t.Error("the probe carried an Authorization header — it runs before the reader knows who is listening")
	}
	if err := ProbeServerGeneration(context.Background(), addr, testGeneration(0xE2)); !errors.Is(err, errProbeMismatch) {
		t.Errorf("ProbeServerGeneration with the wrong generation: err = %v, want errProbeMismatch", err)
	}
}

func TestProbeServerGeneration_UnrelatedListener_Rejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := ProbeServerGeneration(context.Background(), server.Listener.Addr().String(), testGeneration(0xE3))
	if !errors.Is(err, errProbeMismatch) {
		t.Fatalf("probing an unrelated listener: err = %v, want errProbeMismatch", err)
	}
}

func TestProbeServerGeneration_RefusesRedirects(t *testing.T) {
	generation := testGeneration(0xE4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
	}))
	defer server.Close()

	err := ProbeServerGeneration(context.Background(), server.Listener.Addr().String(), generation)
	if err == nil {
		t.Fatal("the probe followed a redirect")
	}
	if strings.Contains(err.Error(), "example.invalid") {
		t.Errorf("error = %q, leaks the redirect target", err)
	}
}

func TestProbeServerGeneration_RejectsRemoteAndMalformedAddresses(t *testing.T) {
	generation := testGeneration(0xE5)
	for _, addr := range []string{"203.0.113.7:3590", "example.com:80", "127.0.0.1:0", ""} {
		if err := ProbeServerGeneration(context.Background(), addr, generation); err == nil {
			t.Errorf("ProbeServerGeneration(%q) = nil, want a refusal before any connection", addr)
		}
	}
	if err := ProbeServerGeneration(context.Background(), "127.0.0.1:3590", "not-a-generation"); !errors.Is(err, errInvalidGeneration) {
		t.Error("ProbeServerGeneration accepted a malformed generation")
	}
}

// ---------------------------------------------------------------------------
// sanitization
// ---------------------------------------------------------------------------

func TestSanitizeFSError_DropsPaths(t *testing.T) {
	const secretPath = "/Users/someone/Library/Application Support/forgectl/docs-servers/deadbeef.json"
	underlying := errors.New("permission denied")

	for _, err := range []error{
		&fs.PathError{Op: "open", Path: secretPath, Err: underlying},
		&os.LinkError{Op: "rename", Old: secretPath, New: secretPath + ".2", Err: underlying},
	} {
		got := sanitizeFSError(err)
		if strings.Contains(got.Error(), secretPath) {
			t.Errorf("sanitizeFSError kept the path: %q", got)
		}
		if !errors.Is(got, underlying) {
			t.Errorf("sanitizeFSError dropped the cause: %v", got)
		}
	}
}

func TestPublishAndDiscoveryErrors_CarryNoSecrets(t *testing.T) {
	const token = "SENTINEL-TOKEN-VALUE"
	dir := filepath.Join(t.TempDir(), "docs-servers")
	rt := testRuntime(t)
	generation := testGeneration(0xF1)
	info := mustInfo(t, "127.0.0.1:3590", generation, time.Unix(1700, 0), token)

	first, err := publishServerInfo(rt, dir, info)
	if err != nil {
		t.Fatalf("publishServerInfo: %v", err)
	}
	defer first.Lease.Close() //nolint:errcheck // best effort

	_, err = publishServerInfo(rt, dir, info)
	if err == nil {
		t.Fatal("expected a collision")
	}
	for _, secret := range []string{token, generation, "127.0.0.1:3590", dir} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("collision error %q leaks %q", err, secret)
		}
	}
}

// ---------------------------------------------------------------------------
// URL construction (unchanged behavior, retained coverage)
// ---------------------------------------------------------------------------

func TestServerInfo_DocURL_BuildsPath(t *testing.T) {
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	got := info.DocURL("docs", "plan.md")
	want := "http://127.0.0.1:3590/doc/docs/plan.md"
	if got != want {
		t.Errorf("DocURL = %q, want %q", got, want)
	}
}

func TestServerInfo_DocURL_EscapesSpecialChars(t *testing.T) {
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	got := info.DocURL("docs", "release notes #3.md")
	if strings.Contains(got, " ") {
		t.Errorf("DocURL = %q, contains a literal space — an unescaped space can truncate the path a browser sends", got)
	}
	if strings.Contains(got, "#3.md") {
		t.Errorf("DocURL = %q, contains a literal '#' — unescaped, it would truncate the path into a URL fragment", got)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("DocURL produced an unparseable URL %q: %v", got, err)
	}
	if u.Path != "/doc/docs/release notes #3.md" {
		t.Errorf("parsed URL path = %q, want the original filename preserved through escaping", u.Path)
	}
}

func TestServerInfo_BaseURL_BuildsIndexURL(t *testing.T) {
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	got := info.BaseURL()
	want := "http://127.0.0.1:3590/"
	if got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// locate client against a real server
// ---------------------------------------------------------------------------

func TestDiscoveryHTTPClient_Locate_SendsTokenAndDecodes(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Query().Get("path")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(locateResponse{Root: "docs", Rel: "plan.md"}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	info := ServerInfo{Addr: server.Listener.Addr().String(), Token: "s3cret"}
	root, rel, err := discoveryHTTPClient{}.Locate(context.Background(), info, "/abs/plan.md")
	if err != nil || root != "docs" || rel != "plan.md" {
		t.Fatalf("Locate = (%q, %q, %v)", root, rel, err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cret")
	}
	if gotPath != "/abs/plan.md" {
		t.Errorf("path query = %q, want %q", gotPath, "/abs/plan.md")
	}
}

func TestDiscoveryHTTPClient_Locate_NotFoundAndUnauthorized(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"not served", http.StatusNotFound, ErrNotServed},
		{"unauthorized", http.StatusUnauthorized, errStaleGeneration},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			info := ServerInfo{Addr: server.Listener.Addr().String()}
			_, _, err := discoveryHTTPClient{}.Locate(context.Background(), info, "/abs/plan.md")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Locate on %d: err = %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestDiscoveryHTTPClient_Locate_CapsTheResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"root":"docs","rel":"`+strings.Repeat("a", maxRecordBytes)+`"}`); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	info := ServerInfo{Addr: server.Listener.Addr().String()}
	client := discoveryHTTPClient{}
	if _, _, err := client.Locate(context.Background(), info, "/abs/plan.md"); !errors.Is(err, errLocateOversize) {
		t.Fatalf("Locate on an oversized body: err = %v, want errLocateOversize", err)
	}
}
