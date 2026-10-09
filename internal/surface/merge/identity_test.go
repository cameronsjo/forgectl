package merge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: name is a literal at every call site
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The identity fixtures are live captures (2026-10-09) of IdentityQuery
// against cameronsjo/forgectl: asked as "CameronSjo/ForgeCtl" for a branch
// that exists, as "cameronsjo/forgectl" for one that does not, and for a
// repository that does not exist.
func TestDecodeIdentity(t *testing.T) {
	got, err := DecodeIdentity(readFixture(t, "identity_exists.json"))
	if err != nil || got != (Identity{NameWithOwner: "cameronsjo/forgectl", DatabaseID: 1252924951, RefExists: true}) {
		t.Fatalf("exists: %+v, %v; want the canonical lowercase name, the id, and the ref", got, err)
	}
	got, err = DecodeIdentity(readFixture(t, "identity_absent.json"))
	if err != nil || got.RefExists || got.DatabaseID != 1252924951 {
		t.Fatalf("absent: %+v, %v; want no ref", got, err)
	}
	if _, err := DecodeIdentity(readFixture(t, "identity_norepo.json")); !errors.Is(err, ErrIdentityResponse) {
		t.Fatalf("no repository: %v, want ErrIdentityResponse", err)
	}
	for name, body := range map[string]string{
		"not JSON":        `nope`,
		"no data":         `{}`,
		"zero id":         `{"data":{"repository":{"databaseId":0,"nameWithOwner":"o/r","ref":null}}}`,
		"no owner":        `{"data":{"repository":{"databaseId":1,"nameWithOwner":"r","ref":null}}}`,
		"dot segment":     `{"data":{"repository":{"databaseId":1,"nameWithOwner":"o/..","ref":null}}}`,
		"odd character":   `{"data":{"repository":{"databaseId":1,"nameWithOwner":"o/r x","ref":null}}}`,
		"errors and data": `{"data":{"repository":{"databaseId":1,"nameWithOwner":"o/r","ref":null}},"errors":[{"message":"partial"}]}`,
	} {
		if _, err := DecodeIdentity([]byte(body)); !errors.Is(err, ErrIdentityResponse) {
			t.Errorf("%s: %v, want ErrIdentityResponse", name, err)
		}
	}
}

func TestSplitNameWithOwner(t *testing.T) {
	if o, n, err := SplitNameWithOwner("cameronsjo/forgectl"); err != nil || o != "cameronsjo" || n != "forgectl" {
		t.Fatalf("split = %q %q %v", o, n, err)
	}
	for _, bad := range []string{"", "a", "a/", "/b", "a/b/c", "a/.", "-/b x"} {
		if _, _, err := SplitNameWithOwner(bad); err == nil {
			t.Errorf("%q split without error", bad)
		}
	}
}
