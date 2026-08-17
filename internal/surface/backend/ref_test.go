package backend_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// serverID builds a valid fingerprint for tests that need one but are not
// testing fingerprinting.
func serverID(t *testing.T) backend.ServerID {
	t.Helper()
	id, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/private/tmp/fc/sock",
		Version:  "tmux 3.7b",
		Device:   16777232,
		Inode:    4242,
	})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return id
}

func recoveryTag(t *testing.T) backend.RecoveryTag {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatalf("NewRecoveryTag: %v", err)
	}
	return tag
}

// tmuxRef builds a complete, valid tmux reference.
func tmuxRef(t *testing.T) (backend.Ref, backend.RecoveryTag) {
	t.Helper()
	tag := recoveryTag(t)
	id, err := backend.NewTmuxIdentity(tag.OwnershipName())
	if err != nil {
		t.Fatalf("NewTmuxIdentity: %v", err)
	}
	ref, err := backend.NewTmuxRef(backend.TmuxDefaultServer(), serverID(t), tag, id)
	if err != nil {
		t.Fatalf("NewTmuxRef: %v", err)
	}
	return ref, tag
}

const uuidA = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func cmuxRef(t *testing.T) backend.Ref {
	t.Helper()
	id, err := backend.NewCMuxIdentity(uuidA)
	if err != nil {
		t.Fatalf("NewCMuxIdentity: %v", err)
	}
	ref, err := backend.NewCmuxRef(backend.CmuxEnvServer(), serverID(t), recoveryTag(t), id)
	if err != nil {
		t.Fatalf("NewCmuxRef: %v", err)
	}
	return ref
}

func herdrRef(t *testing.T) backend.Ref {
	t.Helper()
	id, err := backend.NewHerdrIdentity("ws-7719", "tab-1", "pane-1")
	if err != nil {
		t.Fatalf("NewHerdrIdentity: %v", err)
	}
	ref, err := backend.NewHerdrRef(backend.HerdrDefaultConfigServer(), serverID(t), recoveryTag(t), id)
	if err != nil {
		t.Fatalf("NewHerdrRef: %v", err)
	}
	return ref
}

// TestRecoveryTag_IsRandomAndRoundTrips pins the two properties the tag has to
// have: it is unguessable, and its encoded form parses back to the same value.
// The distinctness check is what would catch a constructor that forgot to draw
// entropy and returned a constant, which every other test here would pass over.
func TestRecoveryTag_IsRandomAndRoundTrips(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		tag := recoveryTag(t)
		if !tag.Valid() {
			t.Fatalf("fresh tag %q is not valid", tag)
		}
		if seen[tag.String()] {
			t.Fatalf("NewRecoveryTag repeated %q; it is not drawing entropy", tag)
		}
		seen[tag.String()] = true

		parsed, err := backend.ParseRecoveryTag(tag.String())
		if err != nil || parsed != tag {
			t.Fatalf("ParseRecoveryTag(%q) = %v, %v; want the original tag", tag, parsed, err)
		}
	}
}

// TestRecoveryTag_OwnershipNameCarriesOnlyEntropy is the privacy assertion on
// the one string that reaches a manager's UI, a log, and an operator's error
// message. A name that carried a repository slug would leak what the operator
// is working on to anything that can list workspaces.
func TestRecoveryTag_OwnershipNameCarriesOnlyEntropy(t *testing.T) {
	tag := recoveryTag(t)
	name := tag.OwnershipName()

	suffix, ok := strings.CutPrefix(name, "fc-surface-")
	if !ok {
		t.Fatalf("ownership name %q does not carry the forgectl namespace", name)
	}
	if suffix != tag.String() {
		t.Errorf("ownership name suffix = %q, want the tag %q", suffix, tag)
	}
}

// TestParseRecoveryTag_RefusesAnythingButExactHex keeps the grammar closed. The
// tag is generated here from a fixed byte count, so every other shape is a
// value from somewhere else wearing its name.
func TestParseRecoveryTag_RefusesAnythingButExactHex(t *testing.T) {
	valid := recoveryTag(t).String()

	tests := map[string]string{
		"empty":              "",
		"too short":          valid[:len(valid)-1],
		"too long":           valid + "0",
		"uppercase":          strings.ToUpper(valid),
		"non-hex":            strings.Repeat("z", len(valid)),
		"ownership name":     "fc-surface-" + valid,
		"leading whitespace": " " + valid[1:],
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.ParseRecoveryTag(in); !errors.Is(err, backend.ErrInvalidRecoveryTag) {
				t.Errorf("ParseRecoveryTag(%q) err = %v, want ErrInvalidRecoveryTag", in, err)
			}
		})
	}
}

// TestFingerprint_DistinguishesIncarnations is the whole reason a reference
// binds to a fingerprint rather than an endpoint. A daemon that restarts on
// the same socket reuses its local IDs, so an endpoint-only identity would
// authorize closing workspace 3 of a server that has never heard of it.
func TestFingerprint_DistinguishesIncarnations(t *testing.T) {
	base := backend.IncarnationInput{
		Endpoint: "/private/tmp/fc/sock",
		Version:  "tmux 3.7b",
		Device:   16777232,
		Inode:    4242,
		PID:      900,
	}

	first, err := backend.Fingerprint(base)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	// The same observation must produce the same digest, or a probe could
	// never confirm anything.
	again, err := backend.Fingerprint(base)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if !first.Matches(again) {
		t.Fatal("the same incarnation fingerprinted differently twice")
	}

	restarts := map[string]func(*backend.IncarnationInput){
		"same endpoint, new inode": func(in *backend.IncarnationInput) { in.Inode = 4243 },
		"same endpoint, new device": func(in *backend.IncarnationInput) {
			in.Device = 16777233
		},
		"new server pid":     func(in *backend.IncarnationInput) { in.PID = 901 },
		"new start time":     func(in *backend.IncarnationInput) { in.StartedAtUnixNano = 1 },
		"new change time":    func(in *backend.IncarnationInput) { in.ChangedAtUnixNano = 1 },
		"upgraded backend":   func(in *backend.IncarnationInput) { in.Version = "tmux 3.8" },
		"different endpoint": func(in *backend.IncarnationInput) { in.Endpoint = "/private/tmp/fc/other" },
	}

	for name, mutate := range restarts {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			other, err := backend.Fingerprint(in)
			if err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
			if first.Matches(other) {
				t.Errorf("%s produced the same fingerprint; a close could land on the wrong server", name)
			}
		})
	}
}

// TestFingerprint_FieldBoundariesAreHashed proves the length prefixing does
// something. Concatenating the fields raw would let an endpoint that ends in a
// version string collide with the other split — a collision an attacker who
// controls a socket path could choose.
func TestFingerprint_FieldBoundariesAreHashed(t *testing.T) {
	left, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/tmp/sock", Version: "3.7", Inode: 1,
	})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	right, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/tmp/sock3", Version: ".7", Inode: 1,
	})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if left.Matches(right) {
		t.Error("fields concatenate without a boundary; a chosen endpoint can collide with another incarnation")
	}
}

// TestFingerprint_RefusesIncompleteEvidence keeps a fingerprint from being
// computable over nothing. A digest of an empty observation is still a valid
// 64-character hex string, which is exactly the shape that would sail through
// every downstream check.
func TestFingerprint_RefusesIncompleteEvidence(t *testing.T) {
	tests := map[string]backend.IncarnationInput{
		"no endpoint": {Version: "3.7", Inode: 1},
		"no version":  {Endpoint: "/tmp/sock", Inode: 1},
		"no inode":    {Endpoint: "/tmp/sock", Version: "3.7"},
		"nothing":     {},
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			id, err := backend.Fingerprint(in)
			if !errors.Is(err, backend.ErrInvalidServerID) {
				t.Errorf("Fingerprint err = %v, want ErrInvalidServerID", err)
			}
			if id.Valid() {
				t.Error("refused input still produced a valid server identity")
			}
		})
	}
}

// TestServerID_MatchesRefusesTheZeroValue closes the hole where two
// unfingerprinted servers compare equal to each other and authorize a close.
func TestServerID_MatchesRefusesTheZeroValue(t *testing.T) {
	var zero backend.ServerID
	if zero.Matches(zero) {
		t.Error("two unset server identities matched each other")
	}
	if serverID(t).Matches(zero) {
		t.Error("a real server identity matched an unset one")
	}
}

// TestServerSource_KindIsClosed pins the source-to-backend mapping every
// reference constructor checks. A source that reported the wrong backend would
// let a cmux selection chain authorize a tmux close.
func TestServerSource_KindIsClosed(t *testing.T) {
	tests := []struct {
		source backend.ServerSource
		kind   backend.Kind
		name   string
	}{
		{backend.TmuxDefaultServer(), backend.KindTmux, "tmux-default"},
		{backend.TmuxCurrentServer(), backend.KindTmux, "tmux-current"},
		{backend.CmuxDefaultServer(), backend.KindCmux, "cmux-default"},
		{backend.CmuxEnvServer(), backend.KindCmux, "cmux-env"},
		{backend.HerdrDefaultConfigServer(), backend.KindHerdr, "herdr-default-config"},
		{backend.HerdrEnvConfigServer(), backend.KindHerdr, "herdr-env-config"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.source.Valid() {
				t.Fatal("constructor returned an invalid source")
			}
			if got := tc.source.Kind(); got != tc.kind {
				t.Errorf("Kind() = %v, want %v", got, tc.kind)
			}
			if got := tc.source.String(); got != tc.name {
				t.Errorf("String() = %q, want %q", got, tc.name)
			}
		})
	}

	var zero backend.ServerSource
	if zero.Valid() {
		t.Error("the zero server source reports itself valid")
	}
	if zero.Kind() != backend.KindUnspecified {
		t.Error("the zero server source names a backend")
	}
}

// TestRef_RoundTripsThroughJSON covers all three backends. The encoded form is
// what a recovery workflow persists, so a value that cannot survive the trip
// is a value an operator cannot use later.
func TestRef_RoundTripsThroughJSON(t *testing.T) {
	tmux, _ := tmuxRef(t)
	refs := map[string]backend.Ref{
		"tmux":  tmux,
		"cmux":  cmuxRef(t),
		"herdr": herdrRef(t),
	}

	for name, ref := range refs {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(ref)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			decoded, err := backend.DecodeRef(data)
			if err != nil {
				t.Fatalf("DecodeRef: %v", err)
			}
			if decoded != ref {
				t.Errorf("round trip changed the reference:\n got %#v\nwant %#v", decoded, ref)
			}

			// json.Unmarshal must validate too, or a caller who reaches for
			// the idiomatic API gets the unvalidated path.
			var viaUnmarshal backend.Ref
			if err := json.Unmarshal(data, &viaUnmarshal); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if viaUnmarshal != ref {
				t.Error("json.Unmarshal produced a different reference than DecodeRef")
			}
		})
	}
}

// TestRef_MarshalRefusesAnInvalidValue stops an unusable reference from being
// written somewhere its next reader has no way to recognize as never-real.
func TestRef_MarshalRefusesAnInvalidValue(t *testing.T) {
	var zero backend.Ref
	if _, err := json.Marshal(zero); err == nil {
		t.Error("the zero reference encoded successfully")
	}
}

// TestDecodeRef_FailsClosed is the decoder's rejection table. Each row is a
// value somebody could put in a file: an older or newer build's output, a
// hand-edited field, a reference for one backend relabelled as another.
func TestDecodeRef_FailsClosed(t *testing.T) {
	ref, _ := tmuxRef(t)
	good, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// A control: the fixture itself must decode, or every row below passes for
	// the wrong reason.
	if _, err := backend.DecodeRef(good); err != nil {
		t.Fatalf("the valid fixture does not decode: %v", err)
	}

	tag := ref.Tag().String()
	digest := ref.Server().String()
	session := "fc-surface-" + tag

	tests := map[string]string{
		"empty":                 ``,
		"not an object":         `"nope"`,
		"no version":            `{"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `"}`,
		"future version":        `{"version":2,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `"}`,
		"unknown backend":       `{"version":1,"kind":"screen","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `"}`,
		"unknown source":        `{"version":1,"kind":"tmux","source":"tmux-guessed","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `"}`,
		"source is a path":      `{"version":1,"kind":"tmux","source":"/tmp/tmux-501/default","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `"}`,
		"cross-backend source":  `{"version":1,"kind":"tmux","source":"cmux-env","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `"}`,
		"no server identity":    `{"version":1,"kind":"tmux","source":"tmux-default","server":"","tag":"` + tag + `","session":"` + session + `"}`,
		"short server identity": `{"version":1,"kind":"tmux","source":"tmux-default","server":"abc","tag":"` + tag + `","session":"` + session + `"}`,
		"bad tag":               `{"version":1,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"nope","session":"` + session + `"}`,
		"unknown field":         `{"version":1,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `","focus":true}`,
		"trailing content":      string(good) + `{"version":1}`,

		// The session name must carry this reference's own tag. A reference
		// naming somebody else's session is the one that would kill an
		// operator's live work.
		"foreign session name": `{"version":1,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"my-work"}`,
		"tag/name disagree":    `{"version":1,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"fc-surface-` + strings.Repeat("a", 32) + `"}`,
		"no session":           `{"version":1,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `"}`,

		// Fields belonging to another backend must be refused, not ignored.
		"tmux with a workspace": `{"version":1,"kind":"tmux","source":"tmux-default","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `","workspace":"` + uuidA + `"}`,
		"cmux with a session":   `{"version":1,"kind":"cmux","source":"cmux-env","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `","workspace":"` + uuidA + `"}`,
		"cmux with a pane":      `{"version":1,"kind":"cmux","source":"cmux-env","server":"` + digest + `","tag":"` + tag + `","workspace":"` + uuidA + `","pane":"p1"}`,
		"herdr with a session":  `{"version":1,"kind":"herdr","source":"herdr-default-config","server":"` + digest + `","tag":"` + tag + `","session":"` + session + `","workspace":"ws-1"}`,

		// Backend-specific grammar.
		"cmux workspace is not a uuid": `{"version":1,"kind":"cmux","source":"cmux-env","server":"` + digest + `","tag":"` + tag + `","workspace":"ws-1"}`,
		"cmux uuid uppercase":          `{"version":1,"kind":"cmux","source":"cmux-env","server":"` + digest + `","tag":"` + tag + `","workspace":"` + strings.ToUpper(uuidA) + `"}`,
		"herdr workspace has a colon":  `{"version":1,"kind":"herdr","source":"herdr-default-config","server":"` + digest + `","tag":"` + tag + `","workspace":"ws-1:pane-2"}`,
		"herdr workspace is empty":     `{"version":1,"kind":"herdr","source":"herdr-default-config","server":"` + digest + `","tag":"` + tag + `","workspace":""}`,
		"herdr workspace oversized":    `{"version":1,"kind":"herdr","source":"herdr-default-config","server":"` + digest + `","tag":"` + tag + `","workspace":"` + strings.Repeat("w", 129) + `"}`,
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := backend.DecodeRef([]byte(in))
			if err == nil {
				t.Fatalf("DecodeRef accepted %s: %#v", name, got)
			}
			if got.Valid() {
				t.Error("a refused decode still returned a valid reference")
			}
		})
	}
}

// TestRef_IdentityAccessorsAreKindChecked keeps a caller from reaching for the
// wrong backend's identity and receiving an empty string it might then hand to
// a command.
func TestRef_IdentityAccessorsAreKindChecked(t *testing.T) {
	tmux, tag := tmuxRef(t)

	id, err := tmux.TMuxIdentity()
	if err != nil {
		t.Fatalf("TMuxIdentity on a tmux reference: %v", err)
	}
	if id.Session() != tag.OwnershipName() {
		t.Errorf("session = %q, want %q", id.Session(), tag.OwnershipName())
	}

	if _, err := tmux.CMuxIdentity(); !errors.Is(err, backend.ErrRefKindMismatch) {
		t.Errorf("CMuxIdentity on a tmux reference err = %v, want ErrRefKindMismatch", err)
	}
	if _, err := tmux.HerdrIdentity(); !errors.Is(err, backend.ErrRefKindMismatch) {
		t.Errorf("HerdrIdentity on a tmux reference err = %v, want ErrRefKindMismatch", err)
	}
}

// TestNewHerdrIdentity_AllowsAPartialRef covers the shape the plan is explicit
// about: herdr can create a workspace and then fail the pane step, and that
// reference has to be representable or the workspace cannot be cleaned up.
func TestNewHerdrIdentity_AllowsAPartialRef(t *testing.T) {
	id, err := backend.NewHerdrIdentity("ws-7719", "", "")
	if err != nil {
		t.Fatalf("a workspace-only herdr identity was refused: %v", err)
	}
	if id.Workspace() != "ws-7719" || id.Tab() != "" || id.Pane() != "" {
		t.Errorf("partial identity did not round-trip: %#v", id)
	}
}

// TestNewTmuxIdentity_RefusesForeignSessions is the narrowest and most
// important grammar here. A reference may only ever name a session forgectl
// created, so no decoding path can produce one that authorizes killing the
// operator's own tmux work.
func TestNewTmuxIdentity_RefusesForeignSessions(t *testing.T) {
	tag := recoveryTag(t)

	if _, err := backend.NewTmuxIdentity(tag.OwnershipName()); err != nil {
		t.Fatalf("our own session name was refused: %v", err)
	}

	foreign := []string{
		"",
		"main",
		"0",
		"my-work",
		"fc-surface-",
		"fc-surface-short",
		"fc-surface-" + strings.ToUpper(tag.String()),
		"prefix-fc-surface-" + tag.String(),
		"fc-surface-" + tag.String() + "-extra",
	}
	for _, name := range foreign {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.NewTmuxIdentity(name); !errors.Is(err, backend.ErrInvalidIdentity) {
				t.Errorf("NewTmuxIdentity(%q) err = %v, want ErrInvalidIdentity", name, err)
			}
		})
	}
}

// TestRef_RendersNoSecrets pins what a reference is allowed to say. It reaches
// terminals, logs, and files, so its rendering must carry the backend, the
// selection chain, and the ownership tag — and never a path, a directory, or a
// raw endpoint.
func TestRef_RendersNoSecrets(t *testing.T) {
	ref, tag := tmuxRef(t)

	rendered := ref.String()
	if rendered == "" {
		t.Fatal("a reference rendered nothing; this test would pass vacuously")
	}
	if !strings.Contains(rendered, tag.OwnershipName()) {
		t.Errorf("rendering %q omits the ownership name an operator needs", rendered)
	}

	// Scan everything *except* the ownership name. The tag is 32 random hex
	// characters, and the numeric fixtures below are decimal — which is a
	// subset of hex — so a tag containing "4242" by chance would fail this as
	// a leak roughly once in every few thousand runs.
	scanned := strings.ReplaceAll(rendered, tag.OwnershipName(), "")
	if scanned == rendered {
		t.Fatal("the ownership name was not found to exclude; the scan below covers the wrong text")
	}
	for _, secret := range []string{"/private/tmp/fc/sock", "4242", "16777232"} {
		if strings.Contains(scanned, secret) {
			t.Errorf("rendering %q leaks fingerprint input %q", rendered, secret)
		}
	}
}
