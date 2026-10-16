package mail

import (
	"errors"
	"fmt"
	"path/filepath"
)

func (roster FileRoster) Register(member Worker, replace bool) (Worker, error) {
	if err := ValidateName(member.Name); err != nil {
		return Worker{}, err
	}
	if !filepath.IsAbs(member.Worktree) {
		return Worker{}, errors.New("member worktree must be an absolute path")
	}
	if member.Socket != "" && !filepath.IsAbs(member.Socket) {
		return Worker{}, errors.New("member socket must be an absolute path")
	}
	switch member.Harness {
	case HarnessClaude:
	case HarnessPi:
		if member.Socket == "" {
			return Worker{}, errors.New("pi registration requires an inbox socket")
		}
	case HarnessCodex:
		if member.Socket != "" {
			if err := validateCodexSocketPath(member.Socket); err != nil {
				return Worker{}, err
			}
		}
		if member.ThreadID != "" && !threadPattern.MatchString(member.ThreadID) {
			return Worker{}, errors.New("codex thread id has unexpected characters")
		}
	default:
		return Worker{}, errors.New("member harness must be claude, codex or pi")
	}
	if member.RunID == "" {
		member.RunID = newID()
	} else if !threadPattern.MatchString(member.RunID) {
		return Worker{}, errors.New("run id has unexpected characters")
	}
	member.State = StateUnknown
	err := roster.locked(func(contents *rosterFile) error {
		index := -1
		for position, existing := range contents.Workers {
			if existing.Name == member.Name {
				if !replace {
					return fmt.Errorf("member %s already exists; use --replace to rebind it", quoteTrunc(member.Name))
				}
				index = position
			} else if existing.Coordinator && member.Coordinator {
				return fmt.Errorf("coordinator %s already exists; replace that name instead", quoteTrunc(existing.Name))
			}
		}
		if index >= 0 {
			if contents.Workers[index].Coordinator != member.Coordinator {
				return errors.New("replacement must preserve the member's coordinator role")
			}
			member.Watchers = contents.Workers[index].Watchers
			contents.Workers[index] = member
		} else {
			contents.Workers = append(contents.Workers, member)
		}
		return nil
	})
	return member, err
}
