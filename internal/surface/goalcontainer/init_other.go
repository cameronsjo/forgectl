//go:build !linux

package goalcontainer

import (
	"context"
	"fmt"
	"io"
)

func RunInit(_ context.Context, _ []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "container init is available only inside the approved Linux image")
	return 125
}
