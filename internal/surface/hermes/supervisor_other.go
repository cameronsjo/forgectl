//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package hermes

import (
	"context"
	"errors"
	"io"
)

type Config struct {
	Command      string
	Args         []string
	Model        string
	Ledger       string
	Member       string
	Worktree     string
	Socket       string
	ListenSocket string
	RunID        string
	SessionID    string
	OnSession    func(string) error
	Stdout       io.Writer
	Stderr       io.Writer
}

type Ready struct {
	Type          string `json:"type"`
	Ready         bool   `json:"ready"`
	RunID         string `json:"run_id"`
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	NativeVersion string `json:"native_version"`
	Mode          string `json:"mode"`
	RouteSocket   string `json:"route_socket"`
	ListenSocket  string `json:"listen_socket"`
}

type Request struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

type Response struct {
	V           int    `json:"v"`
	OK          bool   `json:"ok"`
	ID          string `json:"id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Accepted    *bool  `json:"accepted,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
	Idle        *bool  `json:"idle,omitempty"`
	Known       bool   `json:"known,omitempty"`
	Unconfirmed bool   `json:"unconfirmed,omitempty"`
	Retry       bool   `json:"retry,omitempty"`
	Error       string `json:"error,omitempty"`
}

func Run(context.Context, Config) error {
	return errors.New("hermes supervisor requires a Unix platform")
}
