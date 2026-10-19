package goalnative

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type NativeIdentity struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

type NativePrompt struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type NativeEvent struct {
	Identity    NativeIdentity `json:"identity"`
	RequestID   string         `json:"request_id,omitempty"`
	NativeID    string         `json:"native_id,omitempty"`
	Kind        string         `json:"kind"`
	Status      string         `json:"status,omitempty"`
	Text        string         `json:"text,omitempty"`
	InputSHA256 string         `json:"input_sha256,omitempty"`
	Evidence    string         `json:"evidence"`
	Settled     bool           `json:"settled,omitempty"`
}

type NativeSession interface {
	Identity() NativeIdentity
	Submit(context.Context, NativePrompt) error
	Wait(context.Context, string) (TurnResult, error)
	Turn(context.Context, string) (TurnResult, error)
	WaitIdle(context.Context) error
	Close(context.Context) error
}

type NativeConfig struct {
	Binary             string
	SupervisorBinary   string
	ControlDir         string
	Cwd                string
	Socket             string
	Provider           string
	Model              string
	Environment        []string
	ToolSocket         string
	ToolCapabilityFile string
	OnEvent            func(NativeEvent)
	OnRawEvent         func(json.RawMessage)
}

var GoalToolNames = []string{"goal_exec", "goal_read", "goal_write", "goal_mail", "goal_inbox"}

func NewPrompt(text string) (NativePrompt, error) {
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return NativePrompt{}, err
	}
	prompt := NativePrompt{ID: hex.EncodeToString(identifier), Text: text}
	return prompt, ValidatePrompt(prompt)
}

func ValidatePrompt(prompt NativePrompt) error {
	identifier, err := hex.DecodeString(prompt.ID)
	if err != nil || len(identifier) != 16 || hex.EncodeToString(identifier) != prompt.ID {
		return errors.New("native request requires a canonical 128-bit identity")
	}
	if strings.TrimSpace(prompt.Text) == "" || len(prompt.Text) > 128*1024 || strings.HasPrefix(strings.TrimSpace(prompt.Text), "/") {
		return errors.New("native prompt must be bounded non-command text")
	}
	return nil
}

func PromptDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func RunTurn(ctx context.Context, session NativeSession, text string) (TurnResult, error) {
	prompt, err := NewPrompt(text)
	if err != nil {
		return TurnResult{}, err
	}
	if err := session.Submit(ctx, prompt); err != nil {
		return TurnResult{}, err
	}
	return session.Wait(ctx, prompt.ID)
}
