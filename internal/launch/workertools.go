package launch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrWorkerTools reports a [launch.worker] tools list forgectl will not pass
// to a claude worker as --tools.
var ErrWorkerTools = errors.New("launch: [launch.worker] tools")

// workerToolName is the shape of a built-in tool name: Read, Bash, WebFetch.
//
// Claude Code 2.1.305 drops every --tools name it does not know without a
// word, and names are case-sensitive: `--tools read,bash` started a session
// with no tools at all. So forgectl refuses anything that is not a
// capitalized identifier. It does not check names against a list: the
// built-in set changes between versions and machines (the cloud set carries
// Artifact and DesignSync, the desktop set does not), and a list here would
// refuse a tool the installed version has. A misspelled capitalized name
// still drops silently; `forgectl launch which` shows the list to check.
var workerToolName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// workerToolsArg checks tools and returns the --tools value, or "" when the
// key is unset (nil), which keeps every built-in tool.
//
// An empty list is refused rather than passed as `--tools ""`, which turns off
// every built-in tool: a worker with no Read, Edit or Bash cannot do its task,
// and an empty list in a config file reads as "nothing to restrict". The
// special value "default" is refused too: it means the same as leaving the
// key out.
func workerToolsArg(tools []string) (string, error) {
	if tools == nil {
		return "", nil
	}
	if len(tools) == 0 {
		return "", fmt.Errorf("%w: an empty list would leave the worker no built-in tools; remove the key to keep them all", ErrWorkerTools)
	}
	seen := make(map[string]bool, len(tools))
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t == "default" {
			return "", fmt.Errorf("%w: \"default\" is the same as leaving the key out; remove it", ErrWorkerTools)
		}
		if !workerToolName.MatchString(t) {
			return "", fmt.Errorf("%w: %q is not a built-in tool name (a capitalized name like Read or WebFetch); Claude Code drops a name it does not know without a word", ErrWorkerTools, t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return strings.Join(out, ","), nil
}
