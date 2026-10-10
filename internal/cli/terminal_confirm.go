package cli

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// A terminal confirmation asks a person, at the controlling terminal, to type
// "yes" before a command acts for them: surface intake gh before it queues
// work, surface merge before it merges. No flag or variable replaces it.

// terminalConfirm is a confirmation's two terminal checks, as seams.
type terminalConfirm struct {
	stdinIsTerminal func(io.Reader) bool
	openTTY         func() (io.ReadWriteCloser, error)
}

// productionTerminal is the real pair: Cobra's input must be a terminal, and
// /dev/tty must open as one.
func productionTerminal() terminalConfirm {
	return terminalConfirm{stdinIsTerminal: docsReadInputIsTerminal, openTTY: openConfirmTTY}
}

// confirm runs ask on the controlling terminal: ask writes what is to be
// confirmed and the question to out, and reads the answer from in. Both
// checks are required: stdin a terminal says the command was started by a
// person rather than fed by a pipe, and reading /dev/tty rather than stdin
// means text piped or redirected into the command is never taken as the
// answer. noTerminal is the error either check failing returns, exit 2.
func (t terminalConfirm) confirm(stdin io.Reader, noTerminal error, ask func(in io.Reader, out io.Writer) error) error {
	if t.stdinIsTerminal == nil || t.openTTY == nil || !t.stdinIsTerminal(stdin) {
		return WithExitCode(noTerminal, exitUsage)
	}
	tty, err := t.openTTY()
	if err != nil {
		return WithExitCode(noTerminal, exitUsage)
	}
	answer := ask(tty, tty)
	if err := tty.Close(); err != nil && answer == nil {
		return WithExitCode(noTerminal, exitUsage)
	}
	return answer
}

// openConfirmTTY opens the controlling terminal for reading and writing, and
// refuses one that is not a terminal.
func openConfirmTTY() (io.ReadWriteCloser, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if !term.IsTerminal(int(f.Fd())) {
		_ = f.Close() // the refusal below is the result; a close error adds nothing
		return nil, errors.New("/dev/tty is not a terminal")
	}
	return f, nil
}

// maxConfirmAnswer bounds the answer line read from the terminal.
const maxConfirmAnswer = 256

// readYes reads one line from in and reports whether it is exactly "yes"
// once surrounding space is trimmed; end of input before a newline is not.
func readYes(in io.Reader) bool {
	line, err := bufio.NewReader(io.LimitReader(in, maxConfirmAnswer)).ReadString('\n')
	return err == nil && strings.TrimSpace(line) == "yes"
}
