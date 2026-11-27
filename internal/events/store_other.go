// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package events

import (
	"time"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// The store pins its directory with privdir, which is Unix only.

func (s *Store) Watch(string, []Ref) error   { return privdir.ErrUnsupported }
func (s *Store) Unwatch(string, []Ref) error { return privdir.ErrUnsupported }
func (s *Store) Watched() (Watching, error)  { return nil, privdir.ErrUnsupported }
func (s *Store) State(Ref) (PRState, bool, error) {
	return PRState{}, false, privdir.ErrUnsupported
}
func (s *Store) serialPoll(func() error) error                { return privdir.ErrUnsupported }
func (s *Store) Record(time.Time, []PRState) ([]Event, error) { return nil, privdir.ErrUnsupported }
func (s *Store) Feed(string, int64) ([]Event, int64, int, error) {
	return nil, 0, 0, privdir.ErrUnsupported
}
