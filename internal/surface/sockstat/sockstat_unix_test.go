//go:build unix

package sockstat

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// This file exists because the package's only other test is behind
// `//go:build !unix` and CI runs ubuntu and macOS — both unix — so
// `go test ./internal/surface/sockstat/` reported `[no test files]` and nothing
// verified either half of the fail-closed contract anywhere.
//
// The !unix test is correct and stays; it just never executes here. What that
// left uncovered is not only the stub but the REAL implementations, which had no
// direct test at all — only indirect exercise through the adapters' lstat seams,
// where a fake FileInfo stands in for the platform stat this package exists to
// read.
//
// The fail-closed property turns out to be testable on unix after all, which is
// the point: an os.FileInfo whose Sys() is not a *syscall.Stat_t drives exactly
// the same branch the !unix stubs hardcode, and it runs on the platforms CI
// actually uses.

// fakeInfo is an os.FileInfo whose Sys() returns whatever a test needs —
// including nil, which is how a filesystem that cannot report a platform stat
// presents.
type fakeInfo struct{ sys any }

func (fakeInfo) Name() string       { return "cmux.sock" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() fs.FileMode  { return fs.ModeSocket | 0o600 }
func (fakeInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any         { return f.sys }

var _ os.FileInfo = fakeInfo{}

type fakeDirInfo struct {
	sys  any
	mode fs.FileMode
}

func (fakeDirInfo) Name() string        { return "socket-dir" }
func (fakeDirInfo) Size() int64         { return 0 }
func (f fakeDirInfo) Mode() fs.FileMode { return f.mode }
func (fakeDirInfo) ModTime() time.Time  { return time.Unix(0, 0) }
func (f fakeDirInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeDirInfo) Sys() any          { return f.sys }

// TestFillCarriesTheInodeThatDetectsARestart pins the field the whole package
// exists for. The inode is what turns over when a server rebinds the same path,
// so a Fill that dropped it would leave every fingerprint matching across a
// restart — and a reference would then authorize closing an object on a server
// that has never heard of it.
func TestFillCarriesTheInodeThatDetectsARestart(t *testing.T) {
	in := backend.IncarnationInput{Endpoint: "/tmp/cmux.sock", Version: "cmux-socket/2"}
	Fill(&in, fakeInfo{sys: &syscall.Stat_t{Ino: 4242}})

	if in.Inode != 4242 {
		t.Errorf("Inode = %d, want 4242", in.Inode)
	}
	// Two different inodes must not produce the same fingerprint, which is the
	// property the field is carried FOR — asserted here rather than left to the
	// backend package, because it is this function's reason to exist.
	first, err := backend.Fingerprint(in)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	other := backend.IncarnationInput{Endpoint: in.Endpoint, Version: in.Version}
	Fill(&other, fakeInfo{sys: &syscall.Stat_t{Ino: 4243}})
	second, err := backend.Fingerprint(other)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if first.Matches(second) {
		t.Error("two inodes fingerprinted the same; a restart would go undetected")
	}
}

// TestFillLeavesTheInputUntouchedWhenThePlatformCannotAnswer drives the
// fail-closed half on a platform that CI actually runs.
//
// Fingerprint requires a non-zero inode, so leaving the input alone is what
// makes fingerprinting refuse outright rather than mint a digest over an
// incomplete observation. A Fill that wrote a zero or a placeholder would pass a
// test asserting only "did not panic".
func TestFillLeavesTheInputUntouchedWhenThePlatformCannotAnswer(t *testing.T) {
	in := backend.IncarnationInput{Endpoint: "/tmp/cmux.sock", Version: "cmux-socket/2"}
	before := in

	Fill(&in, fakeInfo{sys: nil})

	if in != before {
		t.Errorf("Fill mutated the input with no stat to read: got %+v, want %+v", in, before)
	}
	if _, err := backend.Fingerprint(in); err == nil {
		t.Error("Fingerprint accepted an input Fill could not complete; a reference " +
			"would be minted over an observation that never happened")
	}
}

// TestOwnerUIDReportsTheOwnerAndDeclinesWhenItCannot covers both directions of
// the ownership answer.
//
// The declining half is the one that matters: callers treat ok=false as "cannot
// assert", never as "ours", and a stub returning ok=true would turn every
// ownership check into one that silently passes when it cannot see the owner.
func TestOwnerUIDReportsTheOwnerAndDeclinesWhenItCannot(t *testing.T) {
	if uid, ok := OwnerUID(fakeInfo{sys: &syscall.Stat_t{Uid: 501}}); !ok || uid != 501 {
		t.Errorf("OwnerUID = (%d, %v), want (501, true)", uid, ok)
	}
	uid, ok := OwnerUID(fakeInfo{sys: nil})
	if ok {
		t.Error("OwnerUID reported ok=true with no platform stat to read it from")
	}
	if uid != 0 {
		t.Errorf("OwnerUID = %d, want 0 alongside ok=false", uid)
	}
}

func TestUnsafeDirectoryReasonReportsOnlyKnownConcerns(t *testing.T) {
	tests := map[string]struct {
		info os.FileInfo
		want string
	}{
		"private and ours": {
			info: fakeDirInfo{sys: &syscall.Stat_t{Uid: 501}, mode: fs.ModeDir | 0o700},
		},
		"group writable": {
			info: fakeDirInfo{sys: &syscall.Stat_t{Uid: 501}, mode: fs.ModeDir | 0o720},
			want: "group or world writable",
		},
		"owned by another user": {
			info: fakeDirInfo{sys: &syscall.Stat_t{Uid: 502}, mode: fs.ModeDir | 0o700},
			want: "owned by another user",
		},
		"both concerns": {
			info: fakeDirInfo{sys: &syscall.Stat_t{Uid: 502}, mode: fs.ModeDir | 0o707},
			want: "group or world writable and owned by another user",
		},
		"owner unavailable is not invented as foreign": {
			info: fakeDirInfo{sys: nil, mode: fs.ModeDir | 0o700},
		},
		"not a directory": {
			info: fakeInfo{sys: &syscall.Stat_t{Uid: 502}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := UnsafeDirectoryReason(tt.info, 501); got != tt.want {
				t.Errorf("UnsafeDirectoryReason = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFillCarriesTheChangeTimeAsASecondWitness pins the field cmux and herdr
// gained, and pins it as the PROPERTY rather than as a value.
//
// Asserting that ChangedAtUnixNano is non-zero would pass for a Fill that wrote
// a constant. What matters is that two sockets differing ONLY in change time
// fingerprint differently — which is the whole reason to read it, since cmux and
// herdr had nothing but the inode before it.
//
// The inode is deliberately held equal here. That is the case the second witness
// exists for: a server that rebinds and happens to land on the same inode number
// is exactly when the first witness says nothing.
func TestFillCarriesTheChangeTimeAsASecondWitness(t *testing.T) {
	const inode = 4242
	base := backend.IncarnationInput{Endpoint: "/tmp/herdr.sock", Version: "herdr/20"}

	first := base
	Fill(&first, fakeInfo{sys: statWithChangeTime(inode, 1_000)})
	if first.ChangedAtUnixNano == 0 {
		t.Fatal("Fill read no change time; cmux and herdr gained no second witness")
	}

	second := base
	Fill(&second, fakeInfo{sys: statWithChangeTime(inode, 2_000)})

	if first.Inode != second.Inode {
		t.Fatalf("the fixtures differ in inode (%d vs %d); this test would pass for the "+
			"wrong reason", first.Inode, second.Inode)
	}

	a, err := backend.Fingerprint(first)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	b, err := backend.Fingerprint(second)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if a.Matches(b) {
		t.Error("two sockets differing only in change time fingerprinted identically; " +
			"a restart onto the same inode would go undetected")
	}
}
