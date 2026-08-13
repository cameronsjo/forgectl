package docs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"time"
)

// Schema and resource bounds for the generation-owned discovery directory.
//
// Every one of these is a deterministic cap rather than a wall-clock threshold:
// the directory is shared mutable state written by any process running as this
// user, so a reader that scanned it unbounded would let a runaway (or hostile)
// writer decide how long `forgectl docs open` takes.
const (
	// docsSchemaVersion is the only record version this build interprets.
	// A record carrying any other value is skipped, never guessed at.
	docsSchemaVersion = 1

	// generationBytes is the crypto/rand width behind a generation name.
	// 128 bits makes an accidental collision between two concurrently
	// starting servers a non-event, which is what lets the publisher treat
	// a collision as a retryable surprise rather than a normal outcome.
	generationBytes  = 16
	generationHexLen = generationBytes * 2

	// maxRecordBytes bounds one serialized record. The real payload is a few
	// hundred bytes; the headroom is for a long bearer token.
	maxRecordBytes = 8 << 10

	// maxTokenBytes matches the bearer-token ceiling --token-file enforces.
	maxTokenBytes = 4096

	// maxDirEntries and maxValidRecords bound the scan. The reader detects
	// cap+1 and fails closed rather than truncating, because truncating would
	// let filesystem enumeration order silently choose which subset of
	// servers is considered — including a subset excluding the live one.
	maxDirEntries   = 256
	maxValidRecords = 64

	// maxConcurrentProbes bounds the freshness fan-out.
	maxConcurrentProbes = 8

	// probeTimeout bounds one freshness probe (or one legacy dial). Every
	// candidate is a locally connectable address, so anything slower than
	// this is not a server the caller wants to wait on.
	probeTimeout = 500 * time.Millisecond

	// discoveryTimeout bounds the whole discovery operation, in addition to
	// whatever deadline the caller's context already carries.
	discoveryTimeout = 5 * time.Second

	// dialTimeout bounds the TCP connect inside a probe or locate request.
	dialTimeout = 250 * time.Millisecond

	// locateTimeout bounds a locate request, which is more generous than a
	// probe because the server may be mid-index-rebuild.
	locateTimeout = 5 * time.Second
)

// ServerInfo is one immutable record published by one running `docs serve`
// generation. It is written once, under a name only that generation will ever
// use, and removed only by the lease that wrote it.
//
// The shape is a v1 wire contract: readers decode into wireServerInfo (text
// fields, strict) and convert, so this struct's Go types never soften a
// malformed record into a plausible one.
type ServerInfo struct {
	// SchemaVersion is exactly docsSchemaVersion for records this build writes.
	SchemaVersion uint `json:"schema_version"`
	// Generation is 32 lowercase hex characters and matches the filename.
	Generation string `json:"generation"`
	// StartedAt is a nonzero UTC instant in canonical RFC3339Nano form. It is
	// the primary sort key: discovery prefers the most recently started server.
	StartedAt time.Time `json:"started_at"`
	// Addr is the advertised, locally connectable host:port, e.g. "127.0.0.1:3590".
	Addr string `json:"addr"`
	// Token is the bearer token the server requires, omitted when it runs
	// without auth (the loopback-only default).
	Token string `json:"token,omitempty"`
}

// DiscoveredServer is a server discovery selected, plus whether it came from
// the pre-generation single-record format. Legacy servers have no freshness
// endpoint, so callers must treat them differently when a token is involved.
type DiscoveredServer struct {
	Info   ServerInfo
	Legacy bool
}

// Publication is a successful publish: a lease that owns exactly the record
// just installed, plus any nonfatal residue.
//
// Warning exists because the final record becomes visible at the install, not
// at the directory sync that follows it. Once the name is installed, a
// directory-sync failure cannot honestly be reported as "there is no record" —
// there is one, and something else may already have read it. So durability
// residue is a warning beside a usable lease, and a plain error means no final
// record exists at all.
type Publication struct {
	Lease   *ServerLease
	Warning error
}

// ErrNoServer indicates no reachable docs server was found — no record
// describes a server that answered, and no legacy record filled the gap.
var ErrNoServer = errors.New("no running docs server found")

// ErrNotServed indicates the running reader does not serve the requested path —
// it is outside every configured root, under an excluded directory, or not a
// markdown file.
var ErrNotServed = errors.New("path is not served by the running docs reader")

// ErrGenerationCollision means the final record name already exists. It is the
// only publication failure worth retrying, and it is reported only for that
// exact case so the serving loop's retry predicate cannot widen by accident.
var ErrGenerationCollision = errors.New("docs discovery generation already exists")

// ErrDiscoveryOverloaded means the discovery directory holds more entries or
// more valid records than the bounded scan will consider. Discovery fails
// closed rather than picking an arbitrary subset; the fix is manual.
var ErrDiscoveryOverloaded = errors.New("docs discovery state exceeds safe limits; stop every `forgectl docs serve` and remove the docs-servers directory")

// Fixed parse and validation failures. None of them carries a path, a record
// body, a generation, an address, or a token: they are counted and surfaced as
// categories, and a hostile filename must never reach a terminal through them.
var (
	errUnknownSchema      = errors.New("docs discovery record uses an unknown schema version")
	errMalformedRecord    = errors.New("docs discovery record is malformed")
	errDuplicateKeys      = errors.New("docs discovery record has duplicate keys")
	errRecordTooLarge     = errors.New("docs discovery record exceeds the size limit")
	errGenerationMismatch = errors.New("docs discovery record generation does not match its filename")
	errInvalidGeneration  = errors.New("docs discovery generation is not 32 lowercase hex characters")
	errInvalidStartedAt   = errors.New("docs discovery record has a noncanonical start time")
	errInvalidToken       = errors.New("docs discovery record has an invalid bearer token")
	errInvalidAddr        = errors.New("docs discovery record has an address that is not locally connectable")
)

// discoveryRuntime is the complete set of effects the discovery package
// performs, passed by value so a test can inject every one of them without a
// mutable package global two concurrent tests would share.
type discoveryRuntime struct {
	now     func() time.Time
	random  io.Reader
	openDir func(path string, create bool) (discoveryDir, error)
	localIP func(netip.Addr) bool
	http    discoveryHTTP
}

// discoveryHTTP is the network surface. Dial is separate from ProbeGeneration
// because a legacy server predates the freshness endpoint: the only liveness
// question that can be asked of it is whether anything is listening.
type discoveryHTTP interface {
	ProbeGeneration(ctx context.Context, addr, generation string) error
	Locate(ctx context.Context, info ServerInfo, absPath string) (root, rel string, err error)
	Dial(ctx context.Context, addr string) error
}

// discoveryDir is a pinned directory handle. Every operation is relative to the
// descriptor opened once at the start, so replacing the directory underneath a
// running server cannot redirect a later write or removal onto another path.
type discoveryDir interface {
	CreateTemp() (discoveryWriteFile, string, error)
	OpenRecord(name string) (io.ReadCloser, error)
	ReadDir(n int) ([]fs.DirEntry, error)
	InstallNoReplace(tempName, finalName string) (warning error, err error)
	Remove(name string) error
	Sync() error
	Close() error
}

type discoveryWriteFile interface {
	io.Writer
	Chmod(fs.FileMode) error
	Sync() error
	Close() error
}

// cryptoRandReader is the entropy source for generations and temp-file names.
// It is a package var rather than a direct crypto/rand.Reader reference so the
// filesystem layer, which has no runtime value in scope, names one source.
var cryptoRandReader io.Reader = rand.Reader

func productionDiscoveryRuntime() discoveryRuntime {
	return discoveryRuntime{
		now:     time.Now,
		random:  rand.Reader,
		openDir: openDiscoveryDir,
		localIP: isLocallyAssignedIP,
		http:    discoveryHTTPClient{},
	}
}

// NewServerInfo mints a record for a server that has already bound addr.
//
// The generation is minted here rather than at publication time because the
// server must be able to answer for it — the freshness endpoint serves this
// exact value, and the self-probe proves the listener knows it — before any
// record naming it becomes visible to a reader.
func NewServerInfo(addr, token string) (ServerInfo, error) {
	return newServerInfo(productionDiscoveryRuntime(), addr, token)
}

func newServerInfo(rt discoveryRuntime, addr, token string) (ServerInfo, error) {
	generation, err := newGeneration(rt.random)
	if err != nil {
		return ServerInfo{}, err
	}
	info := ServerInfo{
		SchemaVersion: docsSchemaVersion,
		Generation:    generation,
		StartedAt:     canonicalTime(rt.now()),
		Addr:          addr,
		Token:         token,
	}
	if err := validateServerInfo(info, rt.localIP); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}

// newGeneration reads exactly generationBytes of entropy. io.ReadFull rather
// than Read because a short read from a degraded entropy source would otherwise
// produce a name with predictable trailing zero bytes.
func newGeneration(random io.Reader) (string, error) {
	raw := make([]byte, generationBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("generate docs discovery generation: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// canonicalTime rounds an instant to what the wire format can carry, so a
// record held in memory sorts identically to the same record read back from
// disk. Without this, a publisher's in-memory StartedAt could compare unequal
// to its own published copy and the selection order would depend on which side
// of the write you looked from.
func canonicalTime(t time.Time) time.Time {
	utc := t.UTC()
	parsed, err := time.Parse(time.RFC3339Nano, utc.Format(time.RFC3339Nano))
	if err != nil {
		return utc
	}
	return parsed
}

// recordFileName is the authoritative v1 filename for a generation.
func recordFileName(generation string) string {
	return generation + ".json"
}

// generationFromFileName recovers the generation from an authoritative v1
// filename, reporting false for every other name — hidden publisher temps,
// leftovers from another tool, and anything a hostile writer chose.
func generationFromFileName(name string) (string, bool) {
	const suffix = ".json"
	if len(name) != generationHexLen+len(suffix) {
		return "", false
	}
	if name[generationHexLen:] != suffix {
		return "", false
	}
	generation := name[:generationHexLen]
	if !validGeneration(generation) {
		return "", false
	}
	return generation, true
}

// validGeneration accepts exactly 32 lowercase hex characters. Uppercase is
// rejected rather than folded: two spellings of one generation would be two
// filenames for one server, and the no-replace install would stop being the
// thing that prevents a second server from claiming a live name.
func validGeneration(s string) bool {
	if len(s) != generationHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validDiscoveryToken implements RFC 6750's b64token grammar:
//
//	1*( ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/" ) *"="
//
// A record whose token would not survive being placed in an Authorization
// header is rejected at parse time rather than at request time, where the
// failure would read as an authentication problem.
func validDiscoveryToken(token string) bool {
	if token == "" || len(token) > maxTokenBytes {
		return false
	}
	i := 0
	for ; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-', c == '.', c == '_', c == '~', c == '+', c == '/':
			continue
		}
		break
	}
	if i == 0 {
		return false
	}
	for ; i < len(token); i++ {
		if token[i] != '=' {
			return false
		}
	}
	return true
}

// validateServerInfo is the single gate every record passes, whether it was
// just minted or just read off disk. Publication revalidates rather than
// trusting its caller, so a bug upstream cannot install a record discovery
// would then refuse to parse.
func validateServerInfo(info ServerInfo, localIP func(netip.Addr) bool) error {
	if info.SchemaVersion != docsSchemaVersion {
		return errUnknownSchema
	}
	if !validGeneration(info.Generation) {
		return errInvalidGeneration
	}
	if info.StartedAt.IsZero() {
		return errInvalidStartedAt
	}
	if !info.StartedAt.Equal(canonicalTime(info.StartedAt)) || info.StartedAt.Location() != time.UTC {
		return errInvalidStartedAt
	}
	if err := validateAdvertisedAddr(info.Addr, localIP); err != nil {
		return err
	}
	if info.Token != "" && !validDiscoveryToken(info.Token) {
		return errInvalidToken
	}
	return nil
}

// sanitizeFSError strips the path out of a filesystem error.
//
// Both *fs.PathError and *os.LinkError embed the full path they failed on, and
// those paths come from a directory any process running as this user can write
// names into. Surfacing one verbatim would put an attacker-chosen string on the
// operator's terminal; the operation and the errno carry everything a diagnosis
// actually needs.
func sanitizeFSError(err error) error {
	if err == nil {
		return nil
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Errorf("%s: %w", linkErr.Op, linkErr.Err)
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %w", pathErr.Op, pathErr.Err)
	}
	return err
}

// DocURL builds the reader URL for a (root, relPath) pair on this server.
// relPath segments are escaped so a space or a '#' in a filename cannot
// truncate or reshape the path.
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
