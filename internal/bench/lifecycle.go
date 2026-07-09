package bench

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// Up brings the configured bench services up through their own contract-exposed
// entrypoints — it never reimplements bring-up. hearth runs its hardened
// scripts/start.sh; chronicle runs its `make sync` target. An unconfigured
// service is skipped with a clear note (not a silent no-op and not a hard
// failure), so a hearth-only machine still works; the command errors only when
// nothing is configured or a configured service's entrypoint fails.
func Up(ctx context.Context, cfg config.Config, runner exec.Runner, notes io.Writer) error {
	brought := 0

	if dir := cfg.Bench.ResolvedHearthDir(); dir != "" {
		fmt.Fprintln(notes, "→ hearth: running scripts/start.sh")
		if err := runner.RunInteractive(ctx, filepath.Join(dir, "scripts", "start.sh")); err != nil {
			return fmt.Errorf("hearth start: %w", err)
		}
		brought++
	} else {
		fmt.Fprintln(notes, "• hearth: skipped (set [bench].hearth_dir or $HEARTH_DIR)")
	}

	if dir := cfg.Bench.ResolvedChronicleDir(); dir != "" {
		fmt.Fprintln(notes, "→ chronicle: running make sync")
		if err := runner.RunInteractive(ctx, "make", "-C", dir, "sync"); err != nil {
			return fmt.Errorf("chronicle sync: %w", err)
		}
		brought++
	} else {
		fmt.Fprintln(notes, "• chronicle: skipped (set [bench].chronicle_dir or $CHRONICLE_DIR)")
	}

	if brought == 0 {
		return fmt.Errorf("no bench services configured; set [bench].hearth_dir and/or chronicle_dir")
	}
	return nil
}

// Open opens a bench UI in the browser. target names a service; an empty target
// defaults to the hearth homepage. URLs are the *.localhost surface Caddy serves;
// the open command is chosen by GOOS. cfg is accepted for symmetry with the rest
// of the bench API and to leave room for config-driven UI hosts.
func Open(ctx context.Context, cfg config.Config, runner exec.Runner, target string) error {
	_ = cfg
	url, ok := benchURL(target)
	if !ok {
		return fmt.Errorf("unknown bench target %q (try: hearth, grafana)", target)
	}
	return runner.RunInteractive(ctx, openCommand(), url)
}

// benchURL maps a target name to its local UI URL. An empty target defaults to
// the hearth homepage.
func benchURL(target string) (string, bool) {
	switch target {
	case "", "hearth":
		return "http://hearth.localhost", true
	case "grafana":
		return "http://grafana.localhost", true
	default:
		return "", false
	}
}

// openCommand is the platform browser-opener: `open` on macOS, `xdg-open`
// elsewhere.
func openCommand() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}
