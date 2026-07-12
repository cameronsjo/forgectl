package workflow

// Verifier gates a workflow file before it is planned. It authenticates the
// bytes the caller has already read (authenticate-before-parse: the run path
// reads the file once, verifies that buffer, then parses the SAME buffer, so a
// swap between check and use is impossible). The interface is scheme-agnostic
// by design — internal/bless supplies the user-presence implementation
// (ADR-0006), and any future backend drops in without touching the executor.
//
// The signature carries data as well as path because verification runs on the
// raw bytes, not a re-read of the file: ADR-0006 corrects ADR-0002's claim that
// #10 needed "no interface change" — it needed exactly one parameter.
type Verifier interface {
	Verify(path string, data []byte) error
}
