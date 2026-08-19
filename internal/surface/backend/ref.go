package backend

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

// refVersion is the wire version of an encoded reference. A decoder that meets
// any other value fails closed rather than guessing which fields it can trust.
const refVersion = 1

// ownershipPrefix namespaces every object forgectl creates in a manager.
//
// It is load-bearing beyond tidiness: a decoded reference must name an object
// this tool created, and the prefix plus a random suffix is what makes that
// checkable. Without it, a reference decoded from a file could authorize
// closing the operator's own long-lived tmux session.
const ownershipPrefix = "fc-surface-"

// recoveryTagBytes is the entropy behind an ownership name. The tag is the
// only handle an operator has on a workspace the daemon created but did not
// report, so it has to be unguessable enough that reconciling on it cannot
// match somebody else's object.
const recoveryTagBytes = 16

// recoveryTagHexLen is the encoded tag length: two hex characters per byte.
const recoveryTagHexLen = recoveryTagBytes * 2

var (
	// ErrInvalidRef reports a reference that cannot be trusted to name an
	// object we created on the server we fingerprinted.
	ErrInvalidRef = errors.New("surface: invalid reference")
	// ErrRefKindMismatch reports an accessor asked for the wrong backend's
	// identity.
	ErrRefKindMismatch = errors.New("surface: reference is for a different backend")
	// ErrInvalidRecoveryTag reports a malformed ownership tag.
	ErrInvalidRecoveryTag = errors.New("surface: invalid recovery tag")
	// ErrInvalidServerID reports an incomplete server fingerprint.
	ErrInvalidServerID = errors.New("surface: incomplete server identity")
	// ErrInvalidIdentity reports a backend object ID that fails its grammar.
	ErrInvalidIdentity = errors.New("surface: invalid backend identity")
)

// serverSourceCode is the closed set of endpoint-selection chains. It is
// unexported so no caller outside this package can cast an integer into a
// source, and so the zero value cannot name a real one.
type serverSourceCode uint8

const (
	sourceInvalid serverSourceCode = iota
	sourceTmuxDefault
	sourceTmuxCurrent
	sourceCmuxDefault
	sourceCmuxEnv
	sourceHerdrDefaultConfig
	sourceHerdrEnvConfig
	sourceCount
)

var sourceNames = [sourceCount]string{
	sourceInvalid:            "invalid",
	sourceTmuxDefault:        "tmux-default",
	sourceTmuxCurrent:        "tmux-current",
	sourceCmuxDefault:        "cmux-default",
	sourceCmuxEnv:            "cmux-env",
	sourceHerdrDefaultConfig: "herdr-default-config",
	sourceHerdrEnvConfig:     "herdr-env-config",
}

var sourceKinds = [sourceCount]Kind{
	sourceInvalid:            KindUnspecified,
	sourceTmuxDefault:        KindTmux,
	sourceTmuxCurrent:        KindTmux,
	sourceCmuxDefault:        KindCmux,
	sourceCmuxEnv:            KindCmux,
	sourceHerdrDefaultConfig: KindHerdr,
	sourceHerdrEnvConfig:     KindHerdr,
}

// ServerSource names *how* an endpoint was selected, never the endpoint
// itself. Close and probe re-run the same selection chain and compare the
// resulting fingerprint; recording a pathname instead would let a reference
// authorize a server the original chain would no longer choose.
//
// It is a struct with an unexported field rather than a string or an exported
// integer type so that the only values in existence are the ones the
// constructors below return, and so its zero value is invalid.
type ServerSource struct{ code serverSourceCode }

// TmuxDefaultServer selects tmux's default server socket.
func TmuxDefaultServer() ServerSource { return ServerSource{code: sourceTmuxDefault} }

// TmuxCurrentServer selects the server named by the inherited TMUX variable.
func TmuxCurrentServer() ServerSource { return ServerSource{code: sourceTmuxCurrent} }

// CmuxDefaultServer selects cmux's default socket.
func CmuxDefaultServer() ServerSource { return ServerSource{code: sourceCmuxDefault} }

// CmuxEnvServer selects the socket named by CMUX_SOCKET_PATH.
func CmuxEnvServer() ServerSource { return ServerSource{code: sourceCmuxEnv} }

// HerdrDefaultConfigServer selects herdr's default config source.
func HerdrDefaultConfigServer() ServerSource {
	return ServerSource{code: sourceHerdrDefaultConfig}
}

// HerdrEnvConfigServer selects the config named by HERDR_CONFIG_PATH.
func HerdrEnvConfigServer() ServerSource { return ServerSource{code: sourceHerdrEnvConfig} }

// Valid reports whether s names a real selection chain.
func (s ServerSource) Valid() bool { return s.code > sourceInvalid && s.code < sourceCount }

// Kind returns the backend this source belongs to, so a constructor can refuse
// a tmux reference carrying a cmux source.
func (s ServerSource) Kind() Kind {
	if !s.Valid() {
		return KindUnspecified
	}
	return sourceKinds[s.code]
}

func (s ServerSource) String() string {
	if s.code >= sourceCount {
		return "invalid(" + strconv.Itoa(int(s.code)) + ")"
	}
	return sourceNames[s.code]
}

// There is deliberately no MarshalText/UnmarshalText pair here. Ref.MarshalJSON
// writes String() and DecodeRef reads it back with parseServerSource, so a
// marshaler would sit off every encoding path — and shipping one without its
// inverse would give a caller who embedded a ServerSource validating encoding
// and non-validating decoding. String and parseServerSource are the one
// spelling pair.

// parseServerSource resolves an exact wire spelling. Unknown values fail
// closed; there is no default source, because falling back to a default server
// is precisely how a close could land on the wrong daemon.
func parseServerSource(s string) (ServerSource, error) {
	for c := sourceInvalid + 1; c < sourceCount; c++ {
		if sourceNames[c] == s {
			return ServerSource{code: c}, nil
		}
	}
	return ServerSource{}, fmt.Errorf("%w: unknown server source", ErrInvalidRef)
}

// RecoveryTag is the random ownership suffix shared by a workspace's native
// name and its reference. It carries no repository name, no path, and no user
// input — only entropy — which is what makes it safe to print in an error and
// safe to match on during reconciliation.
type RecoveryTag struct{ hex string }

// NewRecoveryTag draws a fresh ownership tag.
func NewRecoveryTag() (RecoveryTag, error) {
	buf := make([]byte, recoveryTagBytes)
	if _, err := rand.Read(buf); err != nil {
		return RecoveryTag{}, fmt.Errorf("surface: draw recovery tag: %w", err)
	}
	return RecoveryTag{hex: hex.EncodeToString(buf)}, nil
}

// ParseRecoveryTag validates an encoded tag. The grammar is exact — the tag is
// generated by this package from a fixed byte count, so any other length or
// alphabet is something else wearing its name.
func ParseRecoveryTag(s string) (RecoveryTag, error) {
	if len(s) != recoveryTagHexLen || !lowerHex(s) {
		return RecoveryTag{}, ErrInvalidRecoveryTag
	}
	return RecoveryTag{hex: s}, nil
}

// Valid reports whether t carries a well-formed tag.
func (t RecoveryTag) Valid() bool {
	return len(t.hex) == recoveryTagHexLen && lowerHex(t.hex)
}

func (t RecoveryTag) String() string { return t.hex }

// OwnershipName is the native object name forgectl creates. It contains the
// tag and nothing else: no repository slug, no directory, no display label.
func (t RecoveryTag) OwnershipName() string { return ownershipPrefix + t.hex }

// parseOwnershipName recovers the tag from a native object name, refusing
// anything outside forgectl's namespace.
func parseOwnershipName(name string) (RecoveryTag, error) {
	rest, ok := strings.CutPrefix(name, ownershipPrefix)
	if !ok {
		return RecoveryTag{}, ErrInvalidRecoveryTag
	}
	return ParseRecoveryTag(rest)
}

// ServerID is a fingerprint of the *incarnation* of a server, not of its
// address. A daemon that restarts on the same socket reuses its local IDs, so
// an endpoint-and-protocol hash would happily authorize closing workspace 3 of
// a server that has no idea what workspace 3 used to be.
//
// The strength of that guarantee depends on the evidence an adapter can
// gather: it holds wherever some input actually turns over on a restart. See
// IncarnationInput.ServerReported for the case where the filesystem evidence
// does not — a backend addressed by a config path rather than a socket.
type ServerID struct{ digest string }

// IncarnationInput is the evidence a fingerprint is computed from. The raw
// values stay with the adapter that observed them; only the digest travels in
// a reference.
type IncarnationInput struct {
	// Endpoint is the canonical socket or config path the selection chain
	// resolved to. Required.
	Endpoint string
	// Device and Inode come from lstat of the socket object. Inode is
	// required: it is the field that changes across a restart even when the
	// path does not.
	Device uint64
	Inode  uint64
	// ChangedAtUnixNano is the socket's creation or change timestamp where the
	// platform reports one, and zero where it does not.
	ChangedAtUnixNano int64
	// Version is the backend version or protocol string. Required.
	Version string
	// PID and StartedAtUnixNano are the server process identity where the
	// backend exposes it — tmux does, cmux and herdr do not — and zero
	// otherwise.
	PID               int
	StartedAtUnixNano int64
	// ServerReported is an opaque incarnation token the daemon itself supplies
	// — a boot id, an uptime, a session nonce returned on connect — and is
	// empty when it supplies none.
	//
	// It exists because the filesystem evidence above is only volatile when
	// Endpoint is a socket. For herdr, Endpoint is a *config path*: its inode,
	// device, and change time are all stable across a daemon restart, and
	// herdr reports no pid, so a herdr fingerprint computed from the fields
	// above alone would match across the exact restart it exists to detect.
	// A herdr adapter must populate this (forgectl#344); until it does, the
	// incarnation guarantee stated on ServerID holds for tmux and cmux only.
	ServerReported string
}

// Fingerprint hashes an observed incarnation.
//
// Every field is length-prefixed before hashing so that the boundaries between
// them are part of the digest. Concatenating them raw would let an endpoint
// ending in a version string and an empty version hash identically to the
// other split, which is a collision an attacker with control of a path could
// choose.
func Fingerprint(in IncarnationInput) (ServerID, error) {
	if in.Endpoint == "" || in.Version == "" || in.Inode == 0 {
		return ServerID{}, ErrInvalidServerID
	}
	h := sha256.New()
	writeField(h, []byte(in.Endpoint))
	writeField(h, []byte(in.Version))
	writeField(h, []byte(in.ServerReported))
	writeUint(h, in.Device)
	writeUint(h, in.Inode)
	// The three signed conversions below are reinterpretations, not range
	// checks that were forgotten. A fingerprint needs the raw bit pattern of
	// each timestamp and pid, and two's-complement reinterpretation is
	// injective — a negative value maps to exactly one uint64 and no other
	// input maps there. Range-limiting them would change every digest that
	// already exists and buy nothing.
	writeUint(h, uint64(in.ChangedAtUnixNano)) //nolint:gosec // G115: injective bit reinterpretation, see above
	writeUint(h, uint64(int64(in.PID)))        //nolint:gosec // G115: injective bit reinterpretation, see above
	writeUint(h, uint64(in.StartedAtUnixNano)) //nolint:gosec // G115: injective bit reinterpretation, see above
	return ServerID{digest: hex.EncodeToString(h.Sum(nil))}, nil
}

func writeField(w io.Writer, b []byte) {
	writeUint(w, uint64(len(b)))
	_, _ = w.Write(b)
}

func writeUint(w io.Writer, v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	_, _ = w.Write(buf[:])
}

// parseServerID validates an encoded digest.
func parseServerID(s string) (ServerID, error) {
	if len(s) != sha256.Size*2 || !lowerHex(s) {
		return ServerID{}, ErrInvalidServerID
	}
	return ServerID{digest: s}, nil
}

// Valid reports whether id carries a complete digest.
func (id ServerID) Valid() bool {
	return len(id.digest) == sha256.Size*2 && lowerHex(id.digest)
}

func (id ServerID) String() string { return id.digest }

// Matches reports whether a freshly observed incarnation is the one this
// reference was bound to. It is a plain comparison of digests rather than a
// tolerance check on purpose: "close enough" is not a thing a server identity
// can be.
func (id ServerID) Matches(other ServerID) bool {
	return id.Valid() && other.Valid() && id.digest == other.digest
}

// TmuxIdentity is a tmux session created by forgectl.
type TmuxIdentity struct{ session string }

// NewTmuxIdentity validates a session name.
//
// The grammar is forgectl's own ownership namespace, not tmux's. That is
// deliberately narrow: a tmux reference can only ever name a session this tool
// created, so no decoding path produces one that authorizes killing the
// operator's own work.
//
// That guarantee is tmux-only, and the asymmetry is worth stating plainly
// because it is a guarantee Phase 5 must not assume it inherits. tmux lets us
// choose the session name, so the identity can carry the ownership tag and
// Ref.Validate can check it. A cmux workspace is addressed by a
// server-assigned UUID and a herdr workspace by a server-assigned ID; neither
// can carry our tag, so for those two backends the identity is checked for
// grammar alone and the tag is not bound to the object. See the Closer
// contract for the obligation that fills the gap.
func NewTmuxIdentity(session string) (TmuxIdentity, error) {
	if _, err := parseOwnershipName(session); err != nil {
		return TmuxIdentity{}, fmt.Errorf("%w: tmux session name", ErrInvalidIdentity)
	}
	return TmuxIdentity{session: session}, nil
}

// Session returns the exact tmux session name.
func (i TmuxIdentity) Session() string { return i.session }

// CMuxIdentity is a cmux workspace created by forgectl.
type CMuxIdentity struct{ workspace string }

// NewCMuxIdentity validates a workspace UUID. cmux is asked for UUID output
// mode precisely so this can be an exact grammar rather than a display name.
func NewCMuxIdentity(workspace string) (CMuxIdentity, error) {
	if !validUUID(workspace) {
		return CMuxIdentity{}, fmt.Errorf("%w: cmux workspace uuid", ErrInvalidIdentity)
	}
	return CMuxIdentity{workspace: workspace}, nil
}

// Workspace returns the exact cmux workspace UUID.
func (i CMuxIdentity) Workspace() string { return i.workspace }

// maxHerdrIDLen bounds a herdr identifier. The IDs are server-generated and
// short; the cap exists so a malformed reply cannot put an unbounded string
// into a reference that later reaches a command line.
const maxHerdrIDLen = 128

// HerdrIdentity is a herdr workspace, and optionally the tab and pane inside
// it that a create response reported.
type HerdrIdentity struct{ workspace, tab, pane string }

// NewHerdrIdentity validates a herdr workspace and its optional children.
//
// Only the workspace is required, because the workspace is what close
// authorizes against. Requiring the tab and pane would make a partial ref —
// creation succeeded, the later pane step did not — unrepresentable, and that
// is exactly the case the design needs to be able to clean up rather than
// abandon.
//
// A colon in the workspace ID is refused specifically: herdr's addressing uses
// colons to qualify a workspace with a pane, so a workspace-only ID containing
// one is either a qualified address in the wrong field or an attempt to widen
// what a later command targets. The tab and pane IDs are not colon-checked,
// because they are the qualifiers rather than the thing being qualified, and
// neither is what a close authorizes against.
func NewHerdrIdentity(workspace, tab, pane string) (HerdrIdentity, error) {
	if !validHerdrID(workspace) || strings.Contains(workspace, ":") {
		return HerdrIdentity{}, fmt.Errorf("%w: herdr workspace id", ErrInvalidIdentity)
	}
	if tab != "" && !validHerdrID(tab) {
		return HerdrIdentity{}, fmt.Errorf("%w: herdr tab id", ErrInvalidIdentity)
	}
	if pane != "" && !validHerdrID(pane) {
		return HerdrIdentity{}, fmt.Errorf("%w: herdr pane id", ErrInvalidIdentity)
	}
	return HerdrIdentity{workspace: workspace, tab: tab, pane: pane}, nil
}

// Workspace returns the exact herdr workspace ID.
func (i HerdrIdentity) Workspace() string { return i.workspace }

// Tab returns the tab ID a create response reported, or the empty string.
func (i HerdrIdentity) Tab() string { return i.tab }

// Pane returns the root pane ID a create response reported, or the empty
// string.
func (i HerdrIdentity) Pane() string { return i.pane }

// Ref is a closeable handle on exactly one manager object.
//
// Every field is private and every path into the type validates, so an
// unvalidated value cannot reach Closer or Prober. A reference names the
// backend, the selection chain that found the server, the incarnation of that
// server, the ownership tag, and the object's exact ID — and deliberately
// nothing else. There is no cwd, repository name, prompt, argv, environment,
// nonce, raw socket path, or server pid in here, because a reference is a
// thing that gets persisted and printed.
type Ref struct {
	kind   Kind
	source ServerSource
	server ServerID
	tag    RecoveryTag

	tmux  TmuxIdentity
	cmux  CMuxIdentity
	herdr HerdrIdentity
}

// NewTmuxRef builds a validated tmux reference. The identity's session name
// must carry the same ownership tag as the reference: a mismatch means the
// object we are about to be able to close is not the one we named.
func NewTmuxRef(source ServerSource, server ServerID, tag RecoveryTag, id TmuxIdentity) (Ref, error) {
	r := Ref{kind: KindTmux, source: source, server: server, tag: tag, tmux: id}
	if err := r.Validate(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// NewCmuxRef builds a validated cmux reference.
func NewCmuxRef(source ServerSource, server ServerID, tag RecoveryTag, id CMuxIdentity) (Ref, error) {
	r := Ref{kind: KindCmux, source: source, server: server, tag: tag, cmux: id}
	if err := r.Validate(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// NewHerdrRef builds a validated herdr reference.
func NewHerdrRef(source ServerSource, server ServerID, tag RecoveryTag, id HerdrIdentity) (Ref, error) {
	r := Ref{kind: KindHerdr, source: source, server: server, tag: tag, herdr: id}
	if err := r.Validate(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// Validate reports whether this reference is complete and internally
// consistent. It is the one gate every constructor and the decoder share, so
// there is a single description of what a trustworthy reference is.
func (r Ref) Validate() error {
	if !r.kind.Valid() {
		return fmt.Errorf("%w: unknown backend", ErrInvalidRef)
	}
	if !r.source.Valid() {
		return fmt.Errorf("%w: unset server source", ErrInvalidRef)
	}
	if r.source.Kind() != r.kind {
		return fmt.Errorf("%w: %s source on a %s reference", ErrInvalidRef, r.source, r.kind)
	}
	if !r.server.Valid() {
		return fmt.Errorf("%w: %w", ErrInvalidRef, ErrInvalidServerID)
	}
	if !r.tag.Valid() {
		return fmt.Errorf("%w: %w", ErrInvalidRef, ErrInvalidRecoveryTag)
	}

	// Exactly one identity is populated, and it is the one this kind names. A
	// tmux reference carrying a cmux workspace is not a tmux reference with a
	// stray field; it is a value whose meaning nobody can state.
	set := 0
	if r.tmux != (TmuxIdentity{}) {
		set++
	}
	if r.cmux != (CMuxIdentity{}) {
		set++
	}
	if r.herdr != (HerdrIdentity{}) {
		set++
	}
	if set != 1 {
		return fmt.Errorf("%w: expected exactly one backend identity, found %d", ErrInvalidRef, set)
	}

	switch r.kind {
	case KindTmux:
		if r.tmux == (TmuxIdentity{}) {
			return fmt.Errorf("%w: tmux reference has no session", ErrInvalidRef)
		}
		// The session name embeds the tag, so a reference whose name and tag
		// disagree cannot say which object it owns.
		named, err := parseOwnershipName(r.tmux.session)
		if err != nil || named != r.tag {
			return fmt.Errorf("%w: tmux session name does not carry this reference's tag", ErrInvalidRef)
		}
	case KindCmux:
		if r.cmux == (CMuxIdentity{}) {
			return fmt.Errorf("%w: cmux reference has no workspace", ErrInvalidRef)
		}
	case KindHerdr:
		if r.herdr == (HerdrIdentity{}) {
			return fmt.Errorf("%w: herdr reference has no workspace", ErrInvalidRef)
		}
	case KindUnspecified:
		return fmt.Errorf("%w: unknown backend", ErrInvalidRef)
	}
	return nil
}

// Valid reports whether this reference passes Validate.
func (r Ref) Valid() bool { return r.Validate() == nil }

// Kind returns the backend this reference names.
func (r Ref) Kind() Kind { return r.kind }

// Source returns the selection chain that must be re-run before close or
// probe.
func (r Ref) Source() ServerSource { return r.source }

// Server returns the fingerprint of the incarnation this reference is bound
// to.
func (r Ref) Server() ServerID { return r.server }

// Tag returns the ownership tag.
func (r Ref) Tag() RecoveryTag { return r.tag }

// OwnershipName is the native name forgectl gave this object, and the exact
// string a Closer must find on the object before removing it.
//
// It exists as one accessor so there is one spelling to compare against. For
// tmux the comparison is redundant — Validate already binds the session name
// to the tag — but for cmux and herdr, whose IDs are server-assigned, this is
// the only thing standing between a reference and somebody else's workspace.
func (r Ref) OwnershipName() string { return r.tag.OwnershipName() }

// TMuxIdentity returns the tmux session, or an error if this is not a tmux
// reference. The accessors are kind-checked rather than returning a zero value
// so that a caller reaching for the wrong one gets an error instead of an
// empty string it might then pass to a command.
func (r Ref) TMuxIdentity() (TmuxIdentity, error) {
	if r.kind != KindTmux {
		return TmuxIdentity{}, fmt.Errorf("%w: want tmux, have %s", ErrRefKindMismatch, r.kind)
	}
	return r.tmux, nil
}

// CMuxIdentity returns the cmux workspace, or an error if this is not a cmux
// reference.
func (r Ref) CMuxIdentity() (CMuxIdentity, error) {
	if r.kind != KindCmux {
		return CMuxIdentity{}, fmt.Errorf("%w: want cmux, have %s", ErrRefKindMismatch, r.kind)
	}
	return r.cmux, nil
}

// HerdrIdentity returns the herdr workspace, or an error if this is not a
// herdr reference.
func (r Ref) HerdrIdentity() (HerdrIdentity, error) {
	if r.kind != KindHerdr {
		return HerdrIdentity{}, fmt.Errorf("%w: want herdr, have %s", ErrRefKindMismatch, r.kind)
	}
	return r.herdr, nil
}

// String renders the safe operator-facing form: what to look for, and where.
func (r Ref) String() string {
	if !r.Valid() {
		return "surface reference (invalid)"
	}
	return r.kind.String() + " " + r.tag.OwnershipName() + " via " + r.source.String()
}

func (r Ref) GoString() string { return r.String() }

func (r Ref) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("backend", r.kind.String()),
		slog.String("source", r.source.String()),
		slog.String("tag", r.tag.String()),
	)
}

// refWire is the persisted shape. It is a separate type from Ref so that
// decoding is always a validating construction rather than a field-by-field
// assignment into a value that then looks trustworthy.
type refWire struct {
	Version   int    `json:"version"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Server    string `json:"server"`
	Tag       string `json:"tag"`
	Session   string `json:"session,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Tab       string `json:"tab,omitempty"`
	Pane      string `json:"pane,omitempty"`
}

// MarshalJSON encodes a validated reference. An invalid reference refuses to
// encode: writing one to disk would create a value whose next reader has no
// way to tell it was never real.
func (r Ref) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	w := refWire{
		Version: refVersion,
		Kind:    r.kind.String(),
		Source:  r.source.String(),
		Server:  r.server.String(),
		Tag:     r.tag.String(),
	}
	switch r.kind {
	case KindTmux:
		w.Session = r.tmux.session
	case KindCmux:
		w.Workspace = r.cmux.workspace
	case KindHerdr:
		w.Workspace = r.herdr.workspace
		w.Tab = r.herdr.tab
		w.Pane = r.herdr.pane
	case KindUnspecified:
		return nil, ErrInvalidRef
	}
	return json.Marshal(w)
}

// UnmarshalJSON decodes and validates in one step, so json.Unmarshal into a
// Ref cannot produce an unvalidated value either.
func (r *Ref) UnmarshalJSON(data []byte) error {
	decoded, err := DecodeRef(data)
	if err != nil {
		return err
	}
	*r = decoded
	return nil
}

// DecodeRef parses a persisted reference.
//
// Unknown fields are refused rather than ignored. A reference written by a
// newer build may mean something this build would get wrong, and silently
// dropping the field it turns on is how a close lands somewhere unintended.
func DecodeRef(data []byte) (Ref, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var w refWire
	if err := dec.Decode(&w); err != nil {
		return Ref{}, fmt.Errorf("%w: %w", ErrInvalidRef, err)
	}
	if dec.More() {
		return Ref{}, fmt.Errorf("%w: trailing content after the reference", ErrInvalidRef)
	}
	if w.Version != refVersion {
		return Ref{}, fmt.Errorf("%w: unsupported reference version %d", ErrInvalidRef, w.Version)
	}

	kind, ok := ParseKind(w.Kind)
	if !ok {
		return Ref{}, fmt.Errorf("%w: unknown backend %q", ErrInvalidRef, w.Kind)
	}
	source, err := parseServerSource(w.Source)
	if err != nil {
		return Ref{}, err
	}
	server, err := parseServerID(w.Server)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %w", ErrInvalidRef, err)
	}
	tag, err := ParseRecoveryTag(w.Tag)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %w", ErrInvalidRef, err)
	}

	// Fields belonging to another backend are refused before construction.
	// Validate would catch a populated foreign identity, but not a stray
	// string that never became one — and a decoder that quietly drops a field
	// it did not expect is the same defect as one that ignores an unknown key.
	switch kind {
	case KindTmux:
		if w.Workspace != "" || w.Tab != "" || w.Pane != "" {
			return Ref{}, fmt.Errorf("%w: tmux reference carries workspace fields", ErrInvalidRef)
		}
		id, err := NewTmuxIdentity(w.Session)
		if err != nil {
			return Ref{}, fmt.Errorf("%w: %w", ErrInvalidRef, err)
		}
		return NewTmuxRef(source, server, tag, id)
	case KindCmux:
		if w.Session != "" || w.Tab != "" || w.Pane != "" {
			return Ref{}, fmt.Errorf("%w: cmux reference carries foreign fields", ErrInvalidRef)
		}
		id, err := NewCMuxIdentity(w.Workspace)
		if err != nil {
			return Ref{}, fmt.Errorf("%w: %w", ErrInvalidRef, err)
		}
		return NewCmuxRef(source, server, tag, id)
	case KindHerdr:
		if w.Session != "" {
			return Ref{}, fmt.Errorf("%w: herdr reference carries a session field", ErrInvalidRef)
		}
		id, err := NewHerdrIdentity(w.Workspace, w.Tab, w.Pane)
		if err != nil {
			return Ref{}, fmt.Errorf("%w: %w", ErrInvalidRef, err)
		}
		return NewHerdrRef(source, server, tag, id)
	case KindUnspecified:
	}
	return Ref{}, fmt.Errorf("%w: unknown backend", ErrInvalidRef)
}

// lowerHex reports whether s is non-empty and made only of 0-9a-f. Lowercase
// specifically: this package encodes hex, it encodes it lowercase, and
// accepting both cases would mean two spellings of one value.
func lowerHex(s string) bool {
	if s == "" {
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

// validUUID reports whether s is an 8-4-4-4-12 UUID in either hex case.
//
// Either case, because the backend that produces these emits UPPERCASE. This
// function previously accepted lowercase only, and said "canonical lowercase" as
// though that settled it — a grammar asserted from the RFC rather than measured
// against the producer. cmux 0.64.22 answers `workspace create --json` with
// "39D03BE6-9444-4A2B-9C24-ABA8C1126A0A", so the narrow rule rejected every
// workspace id cmux actually mints, which would have made every cmux launch
// report a malformed response and orphan the surface it had just created.
//
// The case is not normalised here and callers must not normalise it either. A
// UUID is an opaque handle minted by the backend and handed straight back to it;
// rewriting bytes we did not choose buys nothing and would make the reference
// disagree with the listing it has to be found in. Callers that COMPARE two of
// these fold the case at the comparison instead.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isDigit := c >= '0' && c <= '9'
			isLower := c >= 'a' && c <= 'f'
			isUpper := c >= 'A' && c <= 'F'
			if !isDigit && !isLower && !isUpper {
				return false
			}
		}
	}
	return true
}

// validHerdrID reports whether s is a bounded ASCII identifier with no
// whitespace and no control characters. herdr does not publish an ID grammar,
// so this is a conservative shape check rather than a claim about its format:
// what it is really for is keeping a control character or an unbounded blob
// out of a value that later becomes a command-line argument.
//
// It deliberately does not refuse a leading "-". That an ID like "--kill-all"
// cannot be parsed as a flag is enforced one layer down, by exec.Opaque, which
// refuses a dynamic operand starting with a dash unless an EndOfOptions
// separator precedes it. Duplicating the rule here would put two spellings of
// it in the codebase; the dependency is named so a Phase 5 adapter author
// knows the separator is load-bearing rather than decorative.
func validHerdrID(s string) bool {
	if s == "" || len(s) > maxHerdrIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
