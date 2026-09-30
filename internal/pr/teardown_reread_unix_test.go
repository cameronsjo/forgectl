//go:build unix

package pr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Test plan for the pinned-handle re-read (forgectl#776)
//
// discardStale, discardRecordOnly and setAsideUndecodableRecord each Lstat the
// member through the pinned sessions-dir handle and then re-read it for the
// byte comparison. beforeTeardownReread swaps the entry in exactly the window
// between the two, which the Lstat cannot cover. Every site must refuse and
// leave the entry where it was:
//
//   [x] a FIFO: refused fast, not blocked on
//   [x] a symlink to the renamed original: refused at the open (ELOOP), even
//       though it stays inside the root and reaches the very same file
//   [x] a same-bytes copy (a new inode): refused on identity, before the bytes
//       (which match) could approve it
//   [x] control: no swap, and the site acts
//
// Mutations that turn it red, at any one site or in rereadPinnedMember:
//   - re-read with a plain root.Open(name): the FIFO case blocks, and a site
//     that skips rereadPinnedMember also skips the seam, so every refusal
//     case sees the site act instead
//   - open through root.OpenFile (the pre-#776 openRegularInRoot, which
//     follows an in-root symlink): the symlink case acts
//   - drop the os.SameFile(info, member.info) check: the copy case acts

// teardownRereadSite is one pinned-handle protocol, staged over a fresh
// member it can act on.
type teardownRereadSite struct {
	name string
	// stage seeds a member and returns it with the call that acts on it.
	stage func(t *testing.T) (breadcrumbMember, func() error)
}

func teardownRereadSites() []teardownRereadSite {
	return []teardownRereadSite{
		{"discardStale", func(t *testing.T) (breadcrumbMember, func() error) {
			f := newStaleFixture(t)
			return f.member, func() error { return f.client.discardStale(f.member) }
		}},
		{"discardRecordOnly", func(t *testing.T) (breadcrumbMember, func() error) {
			c := testClient(t, nil)
			path := seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 7}, PhaseQueued, "")
			member, err := c.resolveBreadcrumbMember(path)
			if err != nil {
				t.Fatalf("resolveBreadcrumbMember: %v", err)
			}
			return member, func() error { return c.discardRecordOnly(member) }
		}},
		{"setAsideUndecodableRecord", func(t *testing.T) (breadcrumbMember, func() error) {
			c := testClient(t, nil)
			path := filepath.Join(c.SessionsDir(), "o-r-8-1.json")
			if err := os.WriteFile(path, []byte("{not a record\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			member, err := c.resolveBreadcrumbEntry(path)
			if err != nil {
				t.Fatalf("resolveBreadcrumbEntry: %v", err)
			}
			return member, func() error {
				_, err := c.setAsideUndecodableRecord(member)
				return err
			}
		}},
	}
}

// swapBeforeReread installs swap as the seam for the rest of the test.
func swapBeforeReread(t *testing.T, swap func(t *testing.T, path string)) {
	t.Helper()
	original := beforeTeardownReread
	t.Cleanup(func() { beforeTeardownReread = original })
	beforeTeardownReread = func(path string) { swap(t, path) }
}

func TestTeardownReread_ASwapAfterTheLstatIsRefused(t *testing.T) {
	swaps := []struct {
		name string
		swap func(t *testing.T, path string)
		// check asserts the refusal is the one this swap is meant to hit.
		check func(t *testing.T, err error)
	}{
		{
			name: "fifo",
			swap: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Error(err)
				}
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, errRecordNotRegular) {
					t.Errorf("err = %v, want errRecordNotRegular", err)
				}
			},
		},
		{
			name: "symlink to the renamed original",
			swap: func(t *testing.T, path string) {
				moved := path + ".moved"
				if err := os.Rename(path, moved); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(filepath.Base(moved), path); err != nil {
					t.Error(err)
				}
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, syscall.ELOOP) {
					t.Errorf("err = %v, want an ELOOP refusal at the open", err)
				}
			},
		},
		{
			name: "same-bytes copy",
			swap: func(t *testing.T, path string) {
				data, err := os.ReadFile(filepath.Clean(path))
				if err != nil {
					t.Error(err)
					return
				}
				// Written beside the original while it is still linked, so the
				// copy cannot recycle its inode.
				tmp := filepath.Clean(path + ".copy")
				if err := os.WriteFile(tmp, data, 0o600); err != nil { //nolint:gosec // G703: a name beside the test's own seeded member
					t.Error(err)
				}
				if err := os.Rename(tmp, path); err != nil {
					t.Error(err)
				}
			},
			check: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "changed identity") {
					t.Errorf("err = %v, want the identity refusal", err)
				}
			},
		},
	}

	for _, site := range teardownRereadSites() {
		t.Run(site.name, func(t *testing.T) {
			for _, sw := range swaps {
				t.Run(sw.name, func(t *testing.T) {
					member, act := site.stage(t)
					swapBeforeReread(t, sw.swap)
					err := mustFailFast(t, site.name, act)
					if err == nil {
						t.Fatalf("%s acted on an entry swapped after its Lstat", site.name)
					}
					sw.check(t, err)
					if _, lerr := os.Lstat(member.path); lerr != nil {
						t.Errorf("the swapped-in entry was removed or moved: %v", lerr)
					}
				})
			}
			t.Run("control: no swap", func(t *testing.T) {
				member, act := site.stage(t)
				called := false
				swapBeforeReread(t, func(*testing.T, string) { called = true })
				if err := mustFailFast(t, site.name, act); err != nil {
					t.Fatalf("%s with no swap: %v", site.name, err)
				}
				if !called {
					t.Error("the re-read seam never ran")
				}
				if _, lerr := os.Lstat(member.path); !errors.Is(lerr, os.ErrNotExist) {
					t.Errorf("the member is still at its name after the site acted: %v", lerr)
				}
			})
		})
	}
}
