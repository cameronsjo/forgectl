//go:build unix

package env

// Test plan for dir_unix.go — the post-resolution swap guards
//
// These pin the control the reviewer found entirely untested: the opens that
// refuse a target which stopped being what resolution proved it was.
//
//   [x] A final component swapped for a SYMLINK is refused, not followed
//   [x] A final component swapped for a FIFO is refused, and does NOT hang
//       (a FIFO's read-only open blocks forever without O_NONBLOCK, so the
//       guard would hang instead of refusing — a denial of service with no
//       error to read)
//   [x] A final component swapped for a DIRECTORY is refused
//   [x] An ordinary regular file still opens — the control can go green
//   [x] openatCreate survives concurrent creation of one name (the darwin
//       spurious-ENOENT behaviour; see its doc comment for the measurement)

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDirPin_OpenRegular_RefusesSwappedEntries(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir, name string)
		want  error
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, dir, name string) {
				outside := filepath.Join(t.TempDir(), "victim")
				if err := os.WriteFile(outside, []byte("SECRET=1\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			},
			want: errIsSymlink,
		},
		{
			name: "fifo",
			setup: func(t *testing.T, dir, name string) {
				if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
					t.Skipf("Mkfifo unsupported: %v", err)
				}
			},
			want: errNotRegular,
		},
		{
			name: "directory",
			setup: func(t *testing.T, dir, name string) {
				if err := os.Mkdir(filepath.Join(dir, name), 0o750); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
			},
			want: errNotRegular,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			pin, err := pinDir(dir)
			if err != nil {
				t.Fatalf("pinDir: %v", err)
			}
			t.Cleanup(pin.close)
			c.setup(t, dir, ".env")

			// Bounded, because the failure mode under test for the FIFO case
			// is a hang rather than a wrong answer. A plain call would turn a
			// regression into a test run that never finishes, which reads as
			// infrastructure trouble rather than as this assertion failing.
			type result struct {
				f   *os.File
				err error
			}
			done := make(chan result, 1)
			go func() {
				f, err := pin.openRegular(".env")
				done <- result{f: f, err: err}
			}()

			select {
			case got := <-done:
				if got.f != nil {
					_ = got.f.Close()
					t.Fatalf("openRegular opened a %s, want a refusal", c.name)
				}
				if got.err == nil {
					t.Fatalf("openRegular returned nil error for a %s", c.name)
				}
				// Compared against the sentinel, not a message: the caller
				// branches on these, so the sentinel is the contract.
				if !isSentinel(got.err, c.want) {
					t.Errorf("openRegular error = %v, want %v", got.err, c.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("openRegular against a %s did not return within 3s — it is blocking, not refusing", c.name)
			}
		})
	}
}

// isSentinel reports whether err is want, without pulling errors.Is into the
// table (the sentinels are compared directly by the production callers too).
func isSentinel(err, want error) bool { return err == want }

func TestDirPin_OpenRegular_OpensAnOrdinaryFile(t *testing.T) {
	dir := t.TempDir()
	pin, err := pinDir(dir)
	if err != nil {
		t.Fatalf("pinDir: %v", err)
	}
	t.Cleanup(pin.close)

	const body = "KEY=value\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The green that makes the three refusals above mean something: a guard
	// that refused everything would satisfy them all.
	f, err := pin.openRegular(".env")
	if err != nil {
		t.Fatalf("openRegular on a regular file: %v", err)
	}
	defer func() { _ = f.Close() }()

	got := make([]byte, len(body))
	if _, err := f.Read(got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != body {
		t.Errorf("content = %q, want %q", got, body)
	}
}

func TestDirPin_PinDir_RefusesASymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	// Not `real`: that shadows a Go predeclared identifier, which the
	// predeclared linter flags and which this repo's config enables.
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if _, err := pinDir(link); err == nil {
		t.Fatal("pinDir followed a symlinked directory, want a refusal")
	}
}

func TestOpenatCreate_ConcurrentSameName(t *testing.T) {
	dir := t.TempDir()
	pin, err := pinDir(dir)
	if err != nil {
		t.Fatalf("pinDir: %v", err)
	}
	t.Cleanup(pin.close)

	// Without openatCreate's retry this fails on darwin roughly 70% of the
	// time with a spurious ENOENT — see its doc comment for the measurement.
	// The rounds are what make a probabilistic failure deterministic enough to
	// gate on.
	const rounds, racers = 40, 4
	var mu sync.Mutex
	var failures []error

	for r := range rounds {
		name := "lock" + string(rune('a'+r%26)) + string(rune('a'+r/26))
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(racers)
		for range racers {
			go func() {
				defer wg.Done()
				<-start
				fd, err := openatCreate(pin.fd, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
				if err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
					return
				}
				_ = unix.Close(fd)
			}()
		}
		close(start)
		wg.Wait()
	}

	if len(failures) > 0 {
		t.Errorf("openatCreate failed %d of %d times; first: %v", len(failures), rounds*racers, failures[0])
	}
}
