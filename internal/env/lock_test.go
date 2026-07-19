package env

// Test plan for lock_unix.go / lock_other.go (Classification: concurrency
// regression, real temp-dir fixtures)
//
// commitSet under concurrent writers
//   [x] Two goroutines calling SetValue against the SAME file with DISTINCT
//       keys, released simultaneously via a barrier, both survive: the file
//       ends up holding BOTH keys, not just whichever rename won the race
//       (the lost-update bug withFileLock exists to close — see
//       lock_unix.go's doc comment)

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestSetValue_ConcurrentDistinctKeys_BothSurvive(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	envPath := filepath.Join(repo, ".env")
	if err := os.WriteFile(envPath, []byte("SEED=0\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	client := NewClient(clip.New(&exec.FakeRunner{}, clip.WithGOOS("darwin")))

	const n = 2
	keys := [n]string{"A", "B"}
	values := [n]string{"1", "2"}
	errs := [n]error{}

	// barrier: both goroutines block on ready/start so their SetValue calls
	// fire as close to simultaneously as the scheduler allows — a sequential
	// pair of calls would never exercise the parse-then-write race at all.
	var ready sync.WaitGroup
	ready.Add(n)
	start := make(chan struct{})
	var done sync.WaitGroup
	done.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			_, err := client.SetValue(repo, ".env", keys[i], values[i], false)
			errs[i] = err
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: SetValue(%s): %v", i, keys[i], err)
		}
	}

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	doc, err := Parse(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for i, key := range keys {
		value, ok := doc.Get(key)
		if !ok {
			t.Errorf("key %s missing from %s after concurrent SetValue — lost update", key, envPath)
			continue
		}
		if value != values[i] {
			t.Errorf("key %s = %q, want %q", key, value, values[i])
		}
	}
}
