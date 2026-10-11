//go:build unix

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusCache(t *testing.T) {
	c := StatusCache{stateBase: t.TempDir()}
	const head = "3afe70e8bff88488d3aebd691ffb943829bf676c"
	if data, err := c.Read(head); err != nil || data != nil {
		t.Fatalf("empty cache: %q, %v", data, err)
	}
	if err := c.Write(head, []byte(`{"head":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if data, err := c.Read(head); err != nil || string(data) != `{"head":"x"}` {
		t.Fatalf("read back: %q, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(c.stateBase, "forgectl", "surface", "status-cache-"+head+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v, %v", info, err)
	}
	for _, bad := range []string{"", "../x", head[:39], "3AFE70E8BFF88488D3AEBD691FFB943829BF676C"} {
		if _, err := c.Read(bad); !errors.Is(err, ErrBadCacheKey) {
			t.Errorf("Read(%q): %v", bad, err)
		}
		if err := c.Write(bad, nil); !errors.Is(err, ErrBadCacheKey) {
			t.Errorf("Write(%q): %v", bad, err)
		}
	}
}
