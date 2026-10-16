package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func newSurfaceTeamCmd() *cobra.Command {
	var ledger string
	command := &cobra.Command{
		Use:          "team",
		Short:        "Enroll an explicit cross-harness team without changing installed tools",
		SilenceUsage: true,
	}
	command.PersistentFlags().StringVar(&ledger, "ledger", "", "absolute private team ledger directory (required)")
	command.AddCommand(newSurfaceTeamRegisterCmd(&ledger))
	command.AddCommand(newSurfaceTeamStatusCmd(&ledger))
	command.AddCommand(newSurfaceTeamExtensionCmd(&ledger))
	return command
}

func privateTeamLedger(ledger string, create bool) (string, error) {
	if !filepath.IsAbs(ledger) {
		return "", errors.New("--ledger must be an absolute path")
	}
	ledger = filepath.Clean(ledger)
	if create {
		if err := os.MkdirAll(ledger, 0o700); err != nil {
			return "", err
		}
	}
	info, err := os.Lstat(ledger)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("team ledger must be a real directory private to its owner (mode 0700)")
	}
	return ledger, nil
}

func teamRegistrationAllowed() error {
	if os.Getenv(mail.EnvWorker) != "" || os.Getenv(mail.EnvDrainWorker) != "" {
		return errors.New("a worker cannot register or replace team members")
	}
	return nil
}

func newSurfaceTeamRegisterCmd(ledger *string) *cobra.Command {
	var member mail.Worker
	var harness string
	var replace, asJSON bool
	command := &cobra.Command{
		Use:   "register <name>",
		Short: "Register a member's native endpoint and print its scoped environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if err := teamRegistrationAllowed(); err != nil {
				return mailUsage(err)
			}
			member.Name = arguments[0]
			member.Harness = mail.Harness(harness)
			info, err := os.Stat(member.Worktree)
			if err != nil || !info.IsDir() {
				return mailUsage(errors.New("--worktree must name an existing directory"))
			}
			directory, err := privateTeamLedger(*ledger, true)
			if err != nil {
				return mailUsage(err)
			}
			registered, err := (mail.FileRoster{Dir: directory}).Register(member, replace)
			if err != nil {
				return mailUsage(err)
			}
			environment := map[string]string{mail.EnvLedger: directory, mail.EnvRunID: registered.RunID}
			if registered.Coordinator {
				environment[mail.EnvCoordinator] = registered.Name
			} else {
				environment[mail.EnvWorker] = registered.Name
			}
			if registered.Harness == mail.HarnessPi {
				environment[mail.EnvInbox] = registered.Socket
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), struct {
					Ledger      string            `json:"ledger"`
					Member      mail.Worker       `json:"member"`
					Environment map[string]string `json:"environment"`
				}{directory, registered, environment})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "registered %s (%s), run %s in %s\n", safeLabel(registered.Name), registered.Harness, registered.RunID, safeLabel(directory))
			return err
		},
	}
	command.Flags().StringVar(&harness, "harness", "", "native harness: claude, codex or pi")
	command.Flags().StringVar(&member.Worktree, "worktree", "", "absolute working directory")
	command.Flags().StringVar(&member.Socket, "socket", "", "absolute native Unix socket path")
	command.Flags().StringVar(&member.ThreadID, "thread", "", "Codex thread ID; omit only before the first thread is known")
	command.Flags().StringVar(&member.RunID, "run-id", "", "explicit process incarnation; omit to generate a fresh one")
	command.Flags().BoolVar(&member.Coordinator, "coordinator", false, "register the team's coordinator")
	command.Flags().BoolVar(&replace, "replace", false, "explicitly replace this member's endpoint")
	command.Flags().BoolVar(&asJSON, "json", false, "print the member and environment as JSON")
	return command
}

func newSurfaceTeamStatusCmd(ledger *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Show the registered endpoints without probing or changing them",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			directory, err := privateTeamLedger(*ledger, false)
			if err != nil {
				return mailUsage(err)
			}
			members, err := (mail.FileRoster{Dir: directory}).List()
			if err != nil {
				return mailError(err)
			}
			if members == nil {
				members = []mail.Worker{}
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), struct {
					Ledger       string        `json:"ledger"`
					PeerMessages bool          `json:"peer_messages"`
					Members      []mail.Worker `json:"members"`
				}{Ledger: directory, Members: members})
			}
			for _, member := range members {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s %s coordinator=%t run=%s\n", safeLabel(member.Name), safeLabel(string(member.Harness)), member.Coordinator, safeLabel(member.RunID)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the registered endpoints as JSON")
	return command
}

func newSurfaceTeamExtensionCmd(ledger *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "extension",
		Short: "Write this build's Pi inbox extension inside the private ledger",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			directory, err := privateTeamLedger(*ledger, true)
			if err != nil {
				return mailUsage(err)
			}
			root, err := os.OpenRoot(directory)
			if err != nil {
				return mailError(err)
			}
			defer func() { _ = root.Close() }()
			path := filepath.Join(directory, "forgectl-inbox.ts")
			file, err := root.OpenFile("forgectl-inbox.ts", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				info, statErr := root.Lstat("forgectl-inbox.ts")
				if statErr != nil || !info.Mode().IsRegular() {
					return mailUsage(errors.New("extension path is not a regular file"))
				}
				contents, readErr := root.ReadFile("forgectl-inbox.ts")
				if readErr != nil || !bytes.Equal(contents, mail.PiExtension) {
					return mailUsage(errors.New("extension already exists with different content; use a new ledger for a different build"))
				}
			} else if err != nil {
				return mailError(err)
			} else {
				_, writeErr := file.Write(mail.PiExtension)
				syncErr := file.Sync()
				closeErr := file.Close()
				if writeErr != nil || syncErr != nil || closeErr != nil {
					return mailError(errors.Join(writeErr, syncErr, closeErr))
				}
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), struct {
					Path string `json:"path"`
				}{path})
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), safeText(path))
			return err
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the extension path as JSON")
	return command
}
