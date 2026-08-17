package backend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	fcexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/fake"
)

// bootstrapLine is a realistic bootstrap: the self-path, the hidden verb, the
// private socket, and a rendezvous nonce. Every substring of it is something
// that must not appear in a log.
const bootstrapLine = "/opt/homebrew/bin/forgectl surface _exec --protocol 1 " +
	"--socket /private/tmp/fc-a1b2/sock --nonce " +
	"beefcafe0123456789abcdef0123456789abcdef0123456789abcdef0beefcaf"

func bootstrap(t *testing.T) backend.BootstrapCommand {
	t.Helper()
	b, err := backend.NewBootstrapCommand(fcexec.Opaque(bootstrapLine))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}
	return b
}

// TestNewBootstrapCommand_RefusesAPlainArgument is the constructor's one
// invariant, and it is not a formality.
//
// A plain argument renders its own value under every verb. Accepting one would
// produce a BootstrapCommand that satisfies this type's contract at the call
// site, looks opaque to a reviewer, and prints the nonce the first time an
// adapter formats its command.
func TestNewBootstrapCommand_RefusesAPlainArgument(t *testing.T) {
	// Both non-opaque constructors are reachable from an adapter package, so
	// both are shapes a bootstrap could wrongly be built from.
	plain := map[string]fcexec.Arg{
		"a backend constant":   fcexec.MustFixed("detach"),
		"an option terminator": fcexec.EndOfOptions(),
		"an unset argument":    {},
	}
	for name, arg := range plain {
		if _, err := backend.NewBootstrapCommand(arg); !errors.Is(err, backend.ErrBootstrapNotOpaque) {
			t.Errorf("%s was accepted as a bootstrap: err = %v", name, err)
		}
	}

	// The control: an opaque argument is accepted, so the refusals above are
	// the check discriminating rather than the constructor refusing everything.
	if _, err := backend.NewBootstrapCommand(fcexec.Opaque(bootstrapLine)); err != nil {
		t.Errorf("an opaque argument was refused: %v", err)
	}
}

// TestBootstrapCommand_RendersRedacted keeps the nonce and socket out of every
// route a value can take out of this process.
func TestBootstrapCommand_RendersRedacted(t *testing.T) {
	b := bootstrap(t)

	buf := &bytes.Buffer{}
	slog.New(slog.NewTextHandler(buf, nil)).Info("starting", "bootstrap", b)

	marshaled, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// The struct-in-a-struct case is the one that actually bites: fmt reaches
	// a value held in an unexported field through reflection, where it will
	// not consult a Stringer.
	type holder struct{ cmd backend.BootstrapCommand }

	renders := map[string]string{
		"String()":         b.String(),
		"%v":               fmt.Sprintf("%v", b),
		"%+v":              fmt.Sprintf("%+v", b),
		"%#v":              fmt.Sprintf("%#v", b),
		"%s":               fmt.Sprintf("%s", b), //nolint:gosimple,staticcheck // exercising the verb is the assertion
		"%q":               fmt.Sprintf("%q", b),
		"in a struct, %v":  fmt.Sprintf("%v", holder{cmd: b}),
		"in a struct, %+v": fmt.Sprintf("%+v", holder{cmd: b}),
		"in a struct, %#v": fmt.Sprintf("%#v", holder{cmd: b}),
		"slog":             buf.String(),
		"json":             string(marshaled),
	}

	for verb, rendered := range renders {
		if rendered == "" {
			t.Errorf("%s rendered nothing; this test would pass vacuously", verb)
			continue
		}
		for _, leak := range []string{"beefcafe", "/private/tmp/fc-a1b2/sock", "_exec", "forgectl surface"} {
			if strings.Contains(rendered, leak) {
				t.Errorf("%s leaked %q: %s", verb, leak, rendered)
			}
		}
	}
}

// TestBootstrapCommand_ForwardsWithoutRevealing pins the type's only exit: the
// value goes on into a sensitive command, still opaque, and nothing comes back
// out as a string. The comparison is by Equal — the same way production code
// would have to — because there is no reveal accessor to assert against.
func TestBootstrapCommand_ForwardsWithoutRevealing(t *testing.T) {
	b := bootstrap(t)

	if !b.Set() {
		t.Fatal("a constructed bootstrap reports itself unset")
	}
	forwarded := b.SensitiveArg()
	if !forwarded.Secret() {
		t.Error("SensitiveArg returned a non-opaque argument")
	}
	if !forwarded.Equal(fcexec.Opaque(bootstrapLine)) {
		t.Error("the forwarded argument is not the value the bootstrap was built from")
	}
	if forwarded.Equal(fcexec.Opaque(bootstrapLine + "x")) {
		t.Error("the forwarded argument compared equal to a value it never held")
	}

	var unset backend.BootstrapCommand
	if unset.Set() {
		t.Error("the zero bootstrap reports itself set")
	}
}

// startSpec builds a valid spec for tests that are not testing construction.
func startSpec(t *testing.T, tag backend.RecoveryTag) backend.StartSpec {
	t.Helper()
	spec, err := backend.NewStartSpec(
		"/Users/someone/Projects/thing", "thing", tag, bootstrap(t))
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}
	return spec
}

// TestNewStartSpec_ValidationTable is what an adapter is guaranteed before it
// runs. Both string checks matter because both reach a manager's command line
// and its UI — and the directory is the one an operator does not choose, since
// a checked-out directory name comes from whoever wrote the repository.
func TestNewStartSpec_ValidationTable(t *testing.T) {
	tag := recoveryTag(t)

	good := startSpec(t, tag)
	if err := good.Validate(); err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	if got := good.OwnershipName(); got != tag.OwnershipName() {
		t.Errorf("OwnershipName() = %q, want %q", got, tag.OwnershipName())
	}
	if got := good.Tag(); got != tag {
		t.Errorf("Tag() = %q, want %q", got, tag)
	}

	tests := map[string]struct {
		cwd, name string
		tag       backend.RecoveryTag
		bootstrap backend.BootstrapCommand
	}{
		"no directory":            {"", "thing", tag, bootstrap(t)},
		"relative directory":      {"Projects/thing", "thing", tag, bootstrap(t)},
		"uncanonical directory":   {"/Users/someone/../someone/thing", "thing", tag, bootstrap(t)},
		"trailing slash":          {"/Users/someone/thing/", "thing", tag, bootstrap(t)},
		"directory with escape":   {"/Users/someone/thing\x1b[31m", "thing", tag, bootstrap(t)},
		"directory with newline":  {"/Users/someone/thing\nmore", "thing", tag, bootstrap(t)},
		"directory invalid utf-8": {"/Users/someone/thing\xff", "thing", tag, bootstrap(t)},
		"oversized directory":     {"/Users/" + strings.Repeat("d", 4096), "thing", tag, bootstrap(t)},
		"no name":                 {"/Users/someone/thing", "", tag, bootstrap(t)},
		"oversized name":          {"/Users/someone/thing", strings.Repeat("n", 129), tag, bootstrap(t)},
		"name with an escape":     {"/Users/someone/thing", "thing\x1b[31m", tag, bootstrap(t)},
		"name with a newline":     {"/Users/someone/thing", "thing\nmore", tag, bootstrap(t)},
		"name with invalid utf-8": {"/Users/someone/thing", "thing\xff", tag, bootstrap(t)},
		"no tag":                  {"/Users/someone/thing", "thing", backend.RecoveryTag{}, bootstrap(t)},
		"no bootstrap":            {"/Users/someone/thing", "thing", tag, backend.BootstrapCommand{}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec, err := backend.NewStartSpec(tc.cwd, tc.name, tc.tag, tc.bootstrap)
			if !errors.Is(err, backend.ErrInvalidStartSpec) {
				t.Errorf("NewStartSpec err = %v, want ErrInvalidStartSpec", err)
			}
			// A refused construction must not yield a spec an adapter would
			// then act on.
			if err := spec.Validate(); err == nil {
				t.Error("a refused construction returned a valid spec")
			}
		})
	}
}

// TestStartSpec_ZeroValueIsRefused covers the shape the unexported fields
// prevent outside this package but that a future in-package caller could still
// write by struct literal.
func TestStartSpec_ZeroValueIsRefused(t *testing.T) {
	var zero backend.StartSpec
	if err := zero.Validate(); !errors.Is(err, backend.ErrInvalidStartSpec) {
		t.Errorf("the zero spec validated: err = %v", err)
	}
}

// TestStartSpec_ForwardsItsValuesOpaquely pins the contract an adapter works
// against: the directory and the label go into a sensitive command without
// being read back, exactly as the bootstrap does.
func TestStartSpec_ForwardsItsValuesOpaquely(t *testing.T) {
	spec := startSpec(t, recoveryTag(t))

	for name, arg := range map[string]fcexec.Arg{"CWD": spec.CWD(), "Name": spec.Name()} {
		if !arg.Secret() {
			t.Errorf("%s() returned a non-opaque argument", name)
		}
	}
	if !spec.CWD().Equal(fcexec.Opaque("/Users/someone/Projects/thing")) {
		t.Error("CWD() does not carry the directory it was built from")
	}
	if spec.CWD().Equal(fcexec.Opaque("/Users/someone/Projects/other")) {
		t.Error("CWD() compared equal to a directory it never held")
	}
	if !spec.Name().Equal(fcexec.Opaque("thing")) {
		t.Error("Name() does not carry the label it was built from")
	}
	if !spec.Bootstrap().Set() {
		t.Error("Bootstrap() lost the bootstrap command")
	}
}

// TestStartSpec_LogsOnlyTheTag pins what may be recorded about a launch. The
// directory and the display name are the two things a manager sees anyway —
// and the two a log must not carry, because a log outlives the workspace and
// travels further than the manager's own UI.
func TestStartSpec_LogsOnlyTheTag(t *testing.T) {
	tag := recoveryTag(t)
	spec, err := backend.NewStartSpec(
		"/Users/someone/Projects/secret-thing", "secret-thing", tag, bootstrap(t))
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}

	buf := &bytes.Buffer{}
	slog.New(slog.NewTextHandler(buf, nil)).Info("starting", "spec", spec)

	logged := buf.String()
	if !strings.Contains(logged, tag.String()) {
		t.Errorf("the log omits the recovery tag, which is the one thing it should carry: %s", logged)
	}
	for _, leak := range []string{"secret-thing", "/Users/someone", "beefcafe"} {
		if strings.Contains(logged, leak) {
			t.Errorf("the log leaked %q: %s", leak, logged)
		}
	}
}

// TestRequireCapabilities_RefusesBeforeAnythingExists is why the check is up
// front. The point of use for close and probe is rollback, and discovering
// there that an adapter cannot close what it created means discovering it
// while holding something that needs closing.
func TestRequireCapabilities_RefusesBeforeAnythingExists(t *testing.T) {
	full, err := backend.RequireCapabilities(fake.New(backend.KindTmux))
	if err != nil {
		t.Fatalf("a complete adapter was refused: %v", err)
	}
	if full.Kind() != backend.KindTmux {
		t.Errorf("Kind() = %v, want tmux", full.Kind())
	}

	tests := map[string]backend.Adapter{
		"nil":               nil,
		"no backend kind":   fake.New(backend.KindUnspecified),
		"cannot clean up":   fake.NewStartOnly(backend.KindCmux),
		"kind out of range": fake.New(backend.Kind(200)),
	}
	for name, adapter := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.RequireCapabilities(adapter); !errors.Is(err, backend.ErrAdapterCapabilities) {
				t.Errorf("RequireCapabilities err = %v, want ErrAdapterCapabilities", err)
			}
		})
	}
}

// TestFakeAdapter_RecordsWhatItWasHanded exercises the test double the later
// phases depend on, and pins the property that makes cleanup assertable: the
// call log is what proves a close happened exactly once, or not at all.
func TestFakeAdapter_RecordsWhatItWasHanded(t *testing.T) {
	ref, tag := tmuxRef(t)
	adapter := fake.New(backend.KindTmux)
	adapter.StartFunc = func(context.Context, backend.StartSpec) backend.StartResult {
		return backend.NewRefKnown(ref)
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		return backend.NewCloseClosed()
	}

	spec := startSpec(t, tag)

	res := adapter.Start(context.Background(), spec)
	if err := res.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	starts := adapter.Starts()
	if len(starts) != 1 {
		t.Fatalf("recorded %d starts, want 1", len(starts))
	}
	// Asserted through Equal, not a reveal accessor: production has no way to
	// read the bootstrap back out, so a test that could would be proving
	// something production cannot rely on.
	if !starts[0].Bootstrap().SensitiveArg().Equal(fcexec.Opaque(bootstrapLine)) {
		t.Error("the adapter was handed a different bootstrap than the one built")
	}

	if len(adapter.Closes()) != 0 {
		t.Error("close ran without being called")
	}
	if got := adapter.Close(context.Background(), ref); got.State() != backend.CloseClosed {
		t.Errorf("Close() = %v, want closed", got.State())
	}
	if closes := adapter.Closes(); len(closes) != 1 || closes[0] != ref {
		t.Errorf("recorded closes = %v, want exactly the reference", closes)
	}
}

// TestFakeAdapter_UnscriptedCallsFailValidation keeps an unexpected call from
// reading as a success. A nil script returns a zero result, and a zero result
// is invalid — so the omission surfaces where the service checks it rather
// than as a silent pass.
func TestFakeAdapter_UnscriptedCallsFailValidation(t *testing.T) {
	adapter := fake.New(backend.KindHerdr)
	ctx := context.Background()
	ref := herdrRef(t)

	if err := adapter.Start(ctx, backend.StartSpec{}).Validate(); err == nil {
		t.Error("an unscripted Start produced a valid result")
	}
	if err := adapter.Close(ctx, ref).Validate(); err == nil {
		t.Error("an unscripted Close produced a valid result")
	}
	if err := adapter.Probe(ctx, ref).Validate(); err == nil {
		t.Error("an unscripted Probe produced a valid result")
	}
}
