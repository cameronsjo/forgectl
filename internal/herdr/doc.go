// Package herdr reads and changes the herdr session the current process runs
// in, by shelling to the herdr CLI through an [exec.Runner].
//
// It is plumbing only. Starting a harness on a pinned server stays in
// internal/surface/herdradapter, which uses the bounded sensitive runner.
//
// Which server a call reaches is herdr's own choice: it follows the inherited
// HERDR_SOCKET_PATH (measured: a nonexistent path yields a server_not_running
// error naming that path). A caller that needs a different server passes a
// Runner whose calls set that variable; the client has no pin option and
// [New] performs no session check. Running [CheckSession] and, before a
// mutation, [CheckFork] first is the caller's duty.
//
// # Failure shapes
//
// herdr fails with exit 1, empty stdout, and {"error":{"code","message"}} on
// stderr, which [exec.Runner] reports as an *[exec.CommandError]. The client
// turns that into an *[Error] carrying the code. Stderr that is truncated, has
// log lines before the JSON, or is not JSON at all stays the wrapped
// *[exec.CommandError]; it never becomes an *[Error] with an empty code.
//
// # Ids
//
// Tab and pane ids renumber when a tab moves between workspaces, and it is not
// known whether a freed id is reused. Re-list after every mutation and resolve
// a target by terminal_id immediately before acting on it; terminal_id is
// stable across moves.
package herdr
