package sealed

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestMap_RunsTheTransformOnce pins that Map runs its transform exactly once,
// at map time, however often the result is revealed, and that a zero Value or
// a zero Transform maps to the zero Value.
//
// Mutation that turns it red: return Value{reveal: func() string { return
// t.apply(v.reveal()) }} from Map (the transform runs on every reveal).
func TestMap_RunsTheTransformOnce(t *testing.T) {
	calls := 0
	counting := Transform{apply: func(s string) string {
		calls++
		return s + "!"
	}}
	got := New("/w/x#(y);").Map(counting)
	if !got.Equal(New("/w/x#(y);!")) {
		t.Fatal("Map did not hold the transformed payload")
	}
	_ = got.reveal()
	_ = got.reveal()
	if calls != 1 {
		t.Fatalf("the transform ran %d times, want exactly once", calls)
	}
	if (Value{}).Map(counting).Set() {
		t.Error("the zero Value mapped to a set Value")
	}
	if New("x").Map(Transform{}).Set() {
		t.Error("the zero Transform mapped to a set Value")
	}
}

// TestPredicates pins the one-bit questions validation asks, including their
// answer for the zero Value, which is false for every one rather than a panic.
//
// Mutation that turns it red: drop the nil check from LeadsWithDash (the zero
// row panics).
func TestPredicates(t *testing.T) {
	tests := []struct {
		name                         string
		v                            Value
		set, present, abs, leadsDash bool
	}{
		{"zero", Value{}, false, false, false, false},
		{"empty", New(""), true, false, false, false},
		{"absolute", New("/usr/bin/tmux"), true, true, true, false},
		{"relative", New("bin/tmux"), true, true, false, false},
		{"dash", New("-rf"), true, true, false, true},
	}
	for _, tt := range tests {
		got := [4]bool{tt.v.Set(), tt.v.Present(), tt.v.IsAbs(), tt.v.LeadsWithDash()}
		want := [4]bool{tt.set, tt.present, tt.abs, tt.leadsDash}
		if got != want {
			t.Errorf("%s: Set, Present, IsAbs, LeadsWithDash = %v, want %v", tt.name, got, want)
		}
	}
}

// TestEqual pins Equal's zero-Value rule: two zero Values are equal, and a
// zero Value equals nothing else, not even a sealed empty string.
func TestEqual(t *testing.T) {
	if !(Value{}).Equal(Value{}) {
		t.Error("two zero Values are not equal")
	}
	if (Value{}).Equal(New("")) || New("").Equal(Value{}) {
		t.Error("a zero Value equals a sealed empty string")
	}
	if !New("a").Equal(New("a")) || New("a").Equal(New("b")) {
		t.Error("Equal does not compare payloads")
	}
}

// TestCommand_FillsTheCmdAndCopiesEnv pins command's assembly: the path and
// argv in order, env copied (never aliased) and followed by each set entry in
// order, and a non-nil Env even when both are empty, since a nil Env would
// make the child inherit the live process environment.
//
// Mutations that turn it red: set cmd.Env = append(env, ...) (the caller's
// backing array is written); leave Env nil when env and set are empty.
func TestCommand_FillsTheCmdAndCopiesEnv(t *testing.T) {
	backing := make([]string, 4)
	backing[0] = "PATH=/usr/bin"
	env := backing[:1]
	cmd := command(New("/bin/tool"), []Value{New("-t"), New("a b")}, env,
		[]EnvVar{{Key: "K1", Value: New("v1")}, {Key: "K2", Value: New("")}})
	if cmd.Path != "/bin/tool" || !slices.Equal(cmd.Args, []string{"/bin/tool", "-t", "a b"}) {
		t.Errorf("Path, Args = %q, %q", cmd.Path, cmd.Args)
	}
	if want := []string{"PATH=/usr/bin", "K1=v1", "K2="}; !slices.Equal(cmd.Env, want) {
		t.Errorf("Env = %q, want %q", cmd.Env, want)
	}
	if backing[1] != "" {
		t.Errorf("command wrote into the caller's env backing array: %q", backing)
	}
	if empty := command(New("/bin/tool"), nil, nil, nil); empty.Env == nil || len(empty.Env) != 0 {
		t.Errorf("Env = %#v for no env and no set; want a non-nil empty slice", empty.Env)
	}
}

// TestStart_FailureNeverCarriesThePath pins that a failed start returns the
// fixed errNotStarted, not os/exec's error, whose text names the path it
// could not start.
//
// Mutation that turns it red: return err from cmd.Start instead of
// errNotStarted.
func TestStart_FailureNeverCarriesThePath(t *testing.T) {
	const path = "/nonexistent/SEALED-PATH-SENTINEL-4f3e"
	proc, err := Start(New(path), []Value{New("SEALED-ARG-SENTINEL")}, nil, nil, nil, nil)
	if proc != nil || !errors.Is(err, errNotStarted) {
		t.Fatalf("Start of a missing path = (%v, %v), want (nil, errNotStarted)", proc, err)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("Start's error renders a payload: %q", err)
	}
}

// TestStart_RefusesARelativePath pins Start's own refusal of a path that is
// not absolute, the defense in depth behind the runner's validate: nothing
// starts, and the error is the fixed errNotStarted.
//
// Mutation that turns it red: drop the IsAbs check in Start (exec.LookPath
// resolves "sh" and a shell starts).
func TestStart_RefusesARelativePath(t *testing.T) {
	proc, err := Start(New("sh"), []Value{New("-c"), New("exit 0")}, nil, nil, nil, nil)
	if proc != nil {
		_ = proc.Wait()
		t.Fatal("Start ran a relative path; it must refuse before any lookup")
	}
	if !errors.Is(err, errNotStarted) {
		t.Fatalf("Start of a relative path = %v, want errNotStarted", err)
	}
}
