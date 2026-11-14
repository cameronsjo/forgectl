// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package backend_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Rex ids as Rex 0.1.0-preview.19 prints them (redacted random bits).
const (
	rexSession = "session:01a1db33-daeb-7000-8000-000000000003"
	rexBlock   = "block:01a1db33-daec-7000-8000-000000000005"
	// rexBlockMS is the 48-bit timestamp rexBlock embeds, 0x01a1db33daec.
	rexBlockMS = 0x01a1db33daec
)

func rexServerID(t *testing.T, instance string) backend.ServerID {
	t.Helper()
	id, err := backend.FingerprintRexServer("unix:///Users/operator/Library/Application%20Support/rex/server.sock",
		"00000000-0000-4000-8000-000000000002", instance)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func rexRef(t *testing.T) backend.Ref {
	t.Helper()
	id, err := backend.NewRexIdentity(rexSession, rexBlock)
	if err != nil {
		t.Fatalf("NewRexIdentity: %v", err)
	}
	ref, err := backend.NewRexRef(backend.RexDefaultServer(), rexServerID(t, "00000000-0000-4000-8000-000000000001"), recoveryTag(t), id)
	if err != nil {
		t.Fatalf("NewRexRef: %v", err)
	}
	return ref
}

func TestKindRexIsInTheClosedSet(t *testing.T) {
	k, ok := backend.ParseKind("rex")
	if !ok || k != backend.KindRex || !k.Valid() || k.String() != "rex" {
		t.Fatalf("ParseKind(rex) = %v, %v", k, ok)
	}
	for _, bad := range []string{"Rex", "REX", "rex ", "rexx"} {
		if _, ok := backend.ParseKind(bad); ok {
			t.Errorf("ParseKind(%q) accepted a spelling outside the closed set", bad)
		}
	}
	s := backend.RexDefaultServer()
	if !s.Valid() || s.Kind() != backend.KindRex || s.String() != "rex-default" {
		t.Errorf("RexDefaultServer = %v kind %v", s, s.Kind())
	}
}

// TestNewRexIdentity_DerivesTheCreationTime pins the UUIDv7 derivation: the
// first 48 bits, the first two hex groups, are the creation time in Unix
// milliseconds.
func TestNewRexIdentity_DerivesTheCreationTime(t *testing.T) {
	id, err := backend.NewRexIdentity(rexSession, rexBlock)
	if err != nil {
		t.Fatal(err)
	}
	if id.CreatedMS() != rexBlockMS || id.Session() != rexSession || id.Block() != rexBlock {
		t.Fatalf("identity = %+v", id)
	}
	// 0x01a1db33daec ms is 2026-11-14 UTC, the day the ids were measured.
	if ms, _ := backend.RexBlockCreatedMS(rexBlock); ms/1000 < 1794600000 || ms/1000 > 1794700000 {
		t.Errorf("creation time %d is not the measurement's", ms)
	}
	if ms, ok := backend.RexSessionCreatedMS(rexSession); !ok || ms != 0x01a1db33daeb {
		t.Errorf("session time = %d, %v", ms, ok)
	}
}

func TestNewRexIdentity_RefusesOtherShapes(t *testing.T) {
	good := strings.TrimPrefix(rexBlock, "block:")
	for name, tc := range map[string][2]string{
		"session as block":    {rexSession, "session:" + good},
		"block as session":    {"block:" + good, rexBlock},
		"no prefix":           {rexSession, good},
		"uppercase":           {rexSession, "block:" + strings.ToUpper(good)},
		"version 4":           {rexSession, "block:01a1db33-daec-4000-8000-000000000005"},
		"bad variant":         {rexSession, "block:01a1db33-daec-7000-c000-000000000005"},
		"zero time":           {rexSession, "block:00000000-0000-7000-8000-000000000005"},
		"short":               {rexSession, "block:01a1db33-daec-7000-8000-00000000005"},
		"dash leading":        {rexSession, "-block:01a1db33-daec-7000-8000-000000000005"},
		"label, not an id":    {rexSession, "shell"},
		"misplaced separator": {rexSession, "block:01a1db33daec-7000-8000-0000-000000000005"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.NewRexIdentity(tc[0], tc[1]); !errors.Is(err, backend.ErrInvalidIdentity) {
				t.Errorf("NewRexIdentity(%q, %q) = %v, want ErrInvalidIdentity", tc[0], tc[1], err)
			}
		})
	}
}

func TestRexRef_RoundTripsAndIsKindChecked(t *testing.T) {
	ref := rexRef(t)
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	back, err := backend.DecodeRef(data)
	if err != nil {
		t.Fatalf("DecodeRef(%s): %v", data, err)
	}
	if back != ref {
		t.Errorf("round trip changed the reference")
	}
	if _, err := ref.HcIdentity(); !errors.Is(err, backend.ErrRefKindMismatch) {
		t.Errorf("HcIdentity on a rex reference = %v", err)
	}
	tmux, _ := tmuxRef(t)
	if _, err := tmux.RexIdentity(); !errors.Is(err, backend.ErrRefKindMismatch) {
		t.Errorf("RexIdentity on a tmux reference = %v", err)
	}
	if _, err := backend.NewRexRef(backend.HcDefaultSessionServer(), rexServerID(t, "i"), recoveryTag(t), backend.RexIdentity{}); err == nil {
		t.Error("a rex reference with an hc source and no identity was built")
	}
}

// TestDecodeRef_RexFailsClosed: a created_ms the block id contradicts, a
// malformed id, another backend's fields on a rex reference, and rex fields
// on another backend's reference are all refused.
//
// Mutation that turns it red: drop the created_ms comparison in DecodeRef.
func TestDecodeRef_RexFailsClosed(t *testing.T) {
	ref := rexRef(t)
	good, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DecodeRef(good); err != nil {
		t.Fatalf("the valid fixture does not decode: %v", err)
	}
	head := `{"version":2,"kind":"rex","source":"rex-default","server":"` + ref.Server().String() + `","tag":"` + ref.Tag().String() + `"`
	ms := strconv.FormatInt(rexBlockMS, 10)
	tmux, tag := tmuxRef(t)
	tmuxHead := `{"version":2,"kind":"tmux","source":"tmux-default","server":"` + tmux.Server().String() + `","tag":"` + tag.String() + `","session":"` + tag.OwnershipName() + `"`
	for name, in := range map[string]string{
		"created_ms disagrees": head + `,"rex_session":"` + rexSession + `","block":"` + rexBlock + `","created_ms":` + ms + `1}`,
		"created_ms missing":   head + `,"rex_session":"` + rexSession + `","block":"` + rexBlock + `"}`,
		"no block":             head + `,"rex_session":"` + rexSession + `","created_ms":` + ms + `}`,
		"no session":           head + `,"block":"` + rexBlock + `","created_ms":` + ms + `}`,
		"block is a label":     head + `,"rex_session":"` + rexSession + `","block":"shell","created_ms":` + ms + `}`,
		"rex with a terminal":  head + `,"rex_session":"` + rexSession + `","block":"` + rexBlock + `","created_ms":` + ms + `,"terminal":"t1"}`,
		"rex with a session":   head + `,"rex_session":"` + rexSession + `","block":"` + rexBlock + `","created_ms":` + ms + `,"session":"x"}`,
		"rex with an hc source": strings.Replace(head, "rex-default", "hc-default-session", 1) +
			`,"rex_session":"` + rexSession + `","block":"` + rexBlock + `","created_ms":` + ms + `}`,
		"tmux with a block":       tmuxHead + `,"block":"` + rexBlock + `"}`,
		"tmux with a rex session": tmuxHead + `,"rex_session":"` + rexSession + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := backend.DecodeRef([]byte(in)); err == nil {
				t.Fatalf("DecodeRef accepted %s: %v", name, got)
			}
		})
	}
}

// TestFingerprintRexServer_BindsTheInstance: another instance_id (a restarted
// server), host or endpoint is another server, and an incomplete identity is
// refused.
func TestFingerprintRexServer_BindsTheInstance(t *testing.T) {
	a := rexServerID(t, "00000000-0000-4000-8000-000000000001")
	if !a.Matches(rexServerID(t, "00000000-0000-4000-8000-000000000001")) {
		t.Fatal("the same server does not match itself")
	}
	if a.Matches(rexServerID(t, "00000000-0000-4000-8000-0000000000ff")) {
		t.Error("a restarted server (new instance_id) matches the old one")
	}
	other, err := backend.FingerprintRexServer("unix:///other.sock", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001")
	if err != nil || a.Matches(other) {
		t.Error("another endpoint matches")
	}
	for _, in := range [][3]string{{"", "h", "i"}, {"e", "", "i"}, {"e", "h", ""}} {
		if _, err := backend.FingerprintRexServer(in[0], in[1], in[2]); !errors.Is(err, backend.ErrInvalidServerID) {
			t.Errorf("FingerprintRexServer(%q) = %v", in, err)
		}
	}
}
