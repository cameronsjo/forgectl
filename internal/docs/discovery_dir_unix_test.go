//go:build darwin || linux

package docs

// Regression tests for the filesystem controls in discovery_dir_unix.go.
//
// They exist because every one of those controls could previously be deleted
// without turning a test red: the rest of the suite exercises malformed and
// oversized records, never a symlink, FIFO, hardlink, or a group-readable
// record or directory. Each test below is written so that removing the single
// check it names makes it fail — that discrimination is the point, not the
// coverage.
//
// The build tag rather than a runtime skip is deliberate: it matches the tag on
// the file under test exactly, so these tests cannot silently skip on a platform
// where the controls are compiled in.
//
// One check is not driven from here. The owner comparison (stat.Uid vs euid)
// needs a file owned by a different user, which an unprivileged test cannot
// create; and the fstat S_IFDIR check is unreachable behind O_DIRECTORY and
// stands as defense in depth.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// validRecordPayload builds a record that would parse and validate cleanly, so
// that when a test's candidate is refused, the refusal is attributable to the
// safety check under test rather than to the payload.
func validRecordPayload(t *testing.T, seed byte, port int) (generation string, payload []byte) {
	t.Helper()
	generation = testGeneration(seed)
	info := mustInfo(t, fmt.Sprintf("127.0.0.1:%d", port), generation, time.Unix(1700, 0), "")
	payload, err := encodeRecord(info)
	if err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}
	return generation, payload
}

// mkDiscoveryDir creates an empty discovery directory with the mode the
// publisher would have given it, defeating the process umask explicitly so the
// test's premise does not depend on the environment it runs in.
func mkDiscoveryDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "docs-servers")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir discovery directory: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod discovery directory: %v", err)
	}
	return dir
}

// TestOpenRecord_RefusesUnsafeRecords drives each record-side control with a
// candidate that is valid in every respect except the one under test.
//
// Both assertions matter and neither subsumes the other. OpenRecord is where the
// control lives, so it is what discriminates: a FIFO whose type check was removed
// still yields an empty read that fails to parse, so a scanRecords-only assertion
// would stay green with the control deleted. scanRecords is where the control has
// to hold in production, so it is asserted too.
func TestOpenRecord_RefusesUnsafeRecords(t *testing.T) {
	tests := []struct {
		name    string
		setUp   func(t *testing.T, dir string) (recordName string)
		wantErr error
	}{
		{
			// O_NOFOLLOW on the openat. Without it the open follows the link and
			// returns a record whose bytes live outside the 0700 directory, so
			// nothing this package checked governs who can write them.
			name: "symlink to a valid record",
			setUp: func(t *testing.T, dir string) string {
				generation, payload := validRecordPayload(t, 0xA0, 3591)
				target := filepath.Join(t.TempDir(), "elsewhere.json")
				if err := os.WriteFile(target, payload, 0o600); err != nil {
					t.Fatalf("write symlink target: %v", err)
				}
				name := recordFileName(generation)
				if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
					t.Fatalf("symlink record: %v", err)
				}
				return name
			},
			wantErr: unix.ELOOP,
		},
		{
			// The S_IFREG check. A FIFO opens fine under O_NONBLOCK and then reads
			// as empty, so without the type check it is merely "malformed" — which
			// is why this case asserts on OpenRecord and not only on the scan.
			name: "FIFO wearing a record name",
			setUp: func(t *testing.T, dir string) string {
				name := recordFileName(testGeneration(0xA1))
				if err := unix.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
					t.Fatalf("mkfifo: %v", err)
				}
				return name
			},
			wantErr: errUnsafeRecord,
		},
		{
			// The Nlink check. The twin is placed OUTSIDE the discovery directory
			// so the only thing wrong with the record is that its bytes are
			// reachable under a name this directory's permissions do not govern.
			name: "hardlinked record",
			setUp: func(t *testing.T, dir string) string {
				generation, payload := validRecordPayload(t, 0xA2, 3592)
				name := recordFileName(generation)
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatalf("write record: %v", err)
				}
				if err := os.Link(path, filepath.Join(t.TempDir(), "twin.json")); err != nil {
					t.Fatalf("hardlink record: %v", err)
				}
				return name
			},
			wantErr: errUnsafeRecord,
		},
		{
			// The mode&0o077 check. 0640 is the realistic accident: a record
			// written under a permissive umask leaks its bearer token to the group.
			name: "group-readable record",
			setUp: func(t *testing.T, dir string) string {
				generation, payload := validRecordPayload(t, 0xA3, 3593)
				name := recordFileName(generation)
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatalf("write record: %v", err)
				}
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatalf("chmod record: %v", err)
				}
				return name
			},
			wantErr: errUnsafeRecord,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := mkDiscoveryDir(t)
			name := tc.setUp(t, dir)

			handle, err := openDiscoveryDir(dir, false)
			if err != nil {
				t.Fatalf("openDiscoveryDir: %v", err)
			}
			defer handle.Close() //nolint:errcheck // read-only

			file, err := handle.OpenRecord(name)
			if err == nil {
				file.Close() //nolint:errcheck // refusing the accepted handle
				t.Fatalf("OpenRecord accepted a %s — the record-side safety check is not engaging", tc.name)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("OpenRecord err = %v, want %v", err, tc.wantErr)
			}

			records, err := scanRecords(testRuntime(t), dir)
			if err != nil {
				t.Fatalf("scanRecords: %v", err)
			}
			if len(records) != 0 {
				t.Fatalf("scanRecords returned %d records from a directory holding only a %s, want 0", len(records), tc.name)
			}
		})
	}
}

// TestOpenDiscoveryDir_RefusesUnsafeDirectories drives the directory-side
// controls. Every case fails at open, before any record is enumerated, which is
// what makes an unsafe directory a closed door rather than a filtered listing.
func TestOpenDiscoveryDir_RefusesUnsafeDirectories(t *testing.T) {
	tests := []struct {
		name    string
		setUp   func(t *testing.T) (path string)
		wantErr error
	}{
		{
			// The stat.Mode&0o077 check. A group-readable, group-executable
			// directory lets another member of the group enumerate record names
			// and stat them, which is the first half of reading a token.
			name: "group-readable directory",
			setUp: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "docs-servers")
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.Chmod(dir, 0o750); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				return dir
			},
			wantErr: errUnsafeDirMode,
		},
		{
			// O_NOFOLLOW|O_DIRECTORY on the leaf. Following the link would put
			// every subsequent record write and lease removal somewhere this code
			// never audited, including a directory another user can write.
			name: "symlinked directory",
			setUp: func(t *testing.T) string {
				base := t.TempDir()
				real := filepath.Join(base, "real")
				if err := os.Mkdir(real, 0o700); err != nil {
					t.Fatalf("mkdir target: %v", err)
				}
				link := filepath.Join(base, "docs-servers")
				if err := os.Symlink(real, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return link
			},
			wantErr: errUnsafeDirType,
		},
		{
			// The ENOTDIR arm of the same refusal: a plain file where the leaf
			// should be. On darwin, O_NOFOLLOW|O_DIRECTORY against a symlink to a
			// directory returns ENOTDIR rather than ELOOP, so this arm is
			// load-bearing there — do not "simplify" the switch to ELOOP only.
			name: "regular file in place of the directory",
			setUp: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "docs-servers")
				if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
					t.Fatalf("write file: %v", err)
				}
				return path
			},
			wantErr: errUnsafeDirType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setUp(t)

			handle, err := openDiscoveryDir(path, false)
			if err == nil {
				handle.Close() //nolint:errcheck // refusing the accepted handle
				t.Fatalf("openDiscoveryDir accepted a %s — the directory-side safety check is not engaging", tc.name)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("openDiscoveryDir err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestPublishServerInfo_UnsafeDirectory_WritesNoTokenAtAll is the fail-closed
// property behind the two directory tests above, stated as the thing an operator
// actually cares about: publishing into a world-readable directory must not put
// the bearer token on disk even momentarily.
//
// A publisher that created the temp first and audited the directory second would
// pass a rejection test and still fail this one, because the token would already
// have been written into a directory anyone can read.
func TestPublishServerInfo_UnsafeDirectory_WritesNoTokenAtAll(t *testing.T) {
	const token = "s3cret-publish-token"

	parent := t.TempDir()
	dir := filepath.Join(parent, "docs-servers")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	info := mustInfo(t, "127.0.0.1:3594", testGeneration(0xB0), time.Unix(1700, 0), token)
	publication, err := publishServerInfo(testRuntime(t), dir, info)
	if !errors.Is(err, errUnsafeDirMode) {
		t.Fatalf("publishServerInfo into a 0755 directory: err = %v, want errUnsafeDirMode", err)
	}
	if publication.Lease != nil {
		t.Error("a refused publication returned a lease")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused publication left %d entries in a world-readable directory, want 0", len(entries))
	}

	// Belt and braces: the token must not appear anywhere under the parent,
	// including under a name the entry check above would not have recognized.
	if err := filepath.Walk(parent, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), token) {
			t.Errorf("the bearer token reached disk at a refused publication (file %q)", fi.Name())
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
}
