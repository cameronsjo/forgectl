//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

var errUnsupportedJSONProcess = errors.New("goal native JSON process is unsupported on this platform")

type ProcessConfig struct {
	Binary           string
	SupervisorBinary string
	ControlDir       string
	Cwd              string
	Socket           string
	Arguments        []string
	Environment      []string
	OnMessage        func(json.RawMessage)
}

type JSONProcess struct {
	done chan struct{}
}

func StartJSONProcess(context.Context, ProcessConfig) (*JSONProcess, error) {
	return nil, errUnsupportedJSONProcess
}

func ensurePrivateDir(string) error { return errUnsupportedJSONProcess }

func writePrivateJSON(string, any) error { return errUnsupportedJSONProcess }

func waitForUnixSocket(context.Context, string, *os.Process, time.Duration) error {
	return errUnsupportedJSONProcess
}

func (process *JSONProcess) Write(context.Context, any) error { return errUnsupportedJSONProcess }

func (process *JSONProcess) Done() <-chan struct{} { return process.done }

func (process *JSONProcess) Err() error { return errUnsupportedJSONProcess }

func (process *JSONProcess) Close(context.Context) error { return errUnsupportedJSONProcess }
