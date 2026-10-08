//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

// TestLoadInventoryTo_CtrlCEndsAsAUserAbort: SIGINT during the wait cancels the
// query and surfaces as huh.ErrUserAborted, which withCancelHandling turns into
// `cancelled`, exit 130. Without the mapping the user got `context canceled`
// and exit 1.
func TestLoadInventoryTo_CtrlCEndsAsAUserAbort(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	prev := inventoryFn
	inventoryFn = func(ctx context.Context, _ *projects.Client) ([]projects.Repo, []string, error) {
		close(started)
		<-release
		return nil, nil, nil
	}
	t.Cleanup(func() { close(release); inventoryFn = prev })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var errb bytes.Buffer
	result := make(chan error, 1)
	go func() {
		_, _, err := loadInventoryTo(cmd, nil, &errb, true)
		result <- err
	}()
	// inventoryFn runs only after the signal context is registered, so the
	// interrupt below is caught and cannot kill the test binary.
	<-started
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, huh.ErrUserAborted) {
			t.Fatalf("err = %v, want huh.ErrUserAborted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loadInventoryTo did not return after SIGINT")
	}
}
