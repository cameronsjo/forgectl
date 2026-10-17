package backend_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func TestHerdrIdentityTerminalRoundTrip(t *testing.T) {
	tag := recoveryTag(t)
	identity, err := backend.NewHerdrIdentity("w2R", "w2R:t1", "w2R:p1")
	if err != nil {
		t.Fatalf("NewHerdrIdentity: %v", err)
	}
	identity, err = identity.WithTerminal("term_1")
	if err != nil {
		t.Fatalf("WithTerminal: %v", err)
	}
	ref, err := backend.NewHerdrRef(backend.HerdrNamedSessionServer(), serverID(t), tag, identity)
	if err != nil {
		t.Fatalf("NewHerdrRef: %v", err)
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded backend.Ref
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := decoded.HerdrIdentity()
	if err != nil {
		t.Fatalf("HerdrIdentity: %v", err)
	}
	if got.Terminal() != "term_1" {
		t.Errorf("terminal = %q, want term_1", got.Terminal())
	}
}

func TestHerdrIdentityRejectsInvalidTerminal(t *testing.T) {
	identity, err := backend.NewHerdrIdentity("w2R", "w2R:t1", "w2R:p1")
	if err != nil {
		t.Fatalf("NewHerdrIdentity: %v", err)
	}
	for _, terminal := range []string{"", "--bad", "term\n1", "term" + string(make([]byte, 129))} {
		if _, err := identity.WithTerminal(terminal); !errors.Is(err, backend.ErrInvalidIdentity) {
			t.Errorf("WithTerminal(%q) error = %v, want ErrInvalidIdentity", terminal, err)
		}
	}
}

func TestHerdrReferenceRejectsForeignTerminalField(t *testing.T) {
	tag := recoveryTag(t)
	server := serverID(t).String()
	raw := []byte(`{"version":2,"kind":"tmux","source":"tmux-default","server":"` + server + `","tag":"` + tag.String() + `","session":"` + tag.OwnershipName() + `","terminal":"term_1"}`)
	var ref backend.Ref
	if err := json.Unmarshal(raw, &ref); !errors.Is(err, backend.ErrInvalidRef) {
		t.Fatalf("Unmarshal foreign terminal error = %v, want ErrInvalidRef", err)
	}
}
