package mail

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Service sends and flushes messages for one coordinator ledger.
type Service struct {
	Box      Mailbox
	Roster   Roster
	Adapters map[Harness]Adapter
	Policy   Policy
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// SendResult is the outcome of one send or attempt.
type SendResult struct {
	ID     string
	Status Status
	Detail string
}

// FlushReport counts what one flush did.
type FlushReport struct {
	Sent, Requeued, Failed, Expired int
	// Skipped is the number of mailbox lines that could not be read.
	Skipped int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) worker(name string) (Worker, error) {
	if name == SystemSender {
		return Worker{Name: SystemSender}, nil
	}
	if err := ValidateName(name); err != nil {
		return Worker{}, err
	}
	return s.Roster.Get(name)
}

// Send checks policy, appends the message, and attempts delivery once. A
// transient failure leaves it queued for the next flush.
func (s *Service) Send(ctx context.Context, from, to, body string, pri Priority) (SendResult, error) {
	if to == SystemSender {
		return SendResult{}, errors.New("forgectl is not a recipient")
	}
	sender, err := s.worker(from)
	if err != nil {
		return SendResult{}, fmt.Errorf("sender: %w", err)
	}
	recip, err := s.worker(to)
	if err != nil {
		return SendResult{}, fmt.Errorf("recipient: %w", err)
	}
	if sender.Name == recip.Name {
		return SendResult{}, errors.New("a worker cannot message itself")
	}
	clean, err := CleanBody(body)
	if err != nil {
		return SendResult{}, err
	}
	if pri == "" {
		pri = PriorityNext
	}
	now := s.now()
	msg := Message{V: 1, ID: newID(), From: sender.Name, To: recip.Name, Body: clean, Priority: pri, CreatedAt: now}

	var res SendResult
	err = s.Box.Locked(func(tx *Tx) error {
		history, err := tx.Load()
		if err != nil {
			return err
		}
		if err := s.Policy.Check(sender, recip, clean, history, now); err != nil {
			return err
		}
		if err := tx.Enqueue(msg, now); err != nil {
			return err
		}
		res, err = s.attempt(ctx, tx, recip, Entry{Msg: msg, Status: StatusQueued})
		return err
	})
	return res, err
}

// attempt tries one delivery and records the outcome.
func (s *Service) attempt(ctx context.Context, tx *Tx, w Worker, e Entry) (SendResult, error) {
	id := e.Msg.ID
	ad, ok := s.Adapters[w.Harness]
	if !ok || ad == nil {
		detail := fmt.Sprintf("no adapter for harness %s", quoteTrunc(string(w.Harness)))
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, true, s.now())
	}
	detail, err := ad.Deliver(ctx, w, e.Msg, Render(e.Msg))
	now := s.now()
	switch {
	case err == nil:
		return SendResult{ID: id, Status: StatusSent, Detail: detail}, tx.Mark(id, StatusSent, detail, true, now)
	case IsRetryable(err):
		detail = oneLine([]byte(err.Error()), 300)
		st := StatusQueued
		if s.Policy.MaxAttempts > 0 && e.Attempts+1 >= s.Policy.MaxAttempts {
			st = StatusFailed
			detail = fmt.Sprintf("gave up after %d attempts: %s", e.Attempts+1, detail)
		}
		return SendResult{ID: id, Status: st, Detail: detail}, tx.Mark(id, st, detail, true, now)
	default:
		detail = oneLine([]byte(err.Error()), 300)
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, true, now)
	}
}

// Flush retries every queued message that is due, expires the ones past the
// TTL, and tells their senders. It is safe to call from any verb.
func (s *Service) Flush(ctx context.Context) (FlushReport, error) {
	var rep FlushReport
	err := s.Box.Locked(func(tx *Tx) error {
		entries, err := tx.Load()
		if err != nil {
			return err
		}
		rep.Skipped = tx.Skipped
		for _, e := range entries {
			if ctx.Err() != nil {
				return nil
			}
			if !e.Pending() {
				continue
			}
			now := s.now()
			if s.Policy.TTL > 0 && now.Sub(e.Msg.CreatedAt) > s.Policy.TTL {
				if err := tx.Mark(e.Msg.ID, StatusExpired, "still undelivered after the TTL", false, now); err != nil {
					return err
				}
				rep.Expired++
				if err := s.noticeLocked(tx, e.Msg.From, fmt.Sprintf("Your message %s to %s expired undelivered: %s", e.Msg.ID, e.Msg.To, e.Detail), now); err != nil {
					return err
				}
				continue
			}
			if now.Before(s.Policy.nextAttempt(e)) {
				continue
			}
			w, err := s.Roster.Get(e.Msg.To)
			if err != nil && !errors.Is(err, ErrUnknownWorker) {
				// An unreadable roster says nothing about the recipient, so
				// the message keeps its place in the queue.
				return fmt.Errorf("roster: %w", err)
			}
			if err != nil {
				if err := tx.Mark(e.Msg.ID, StatusFailed, "recipient is no longer in the roster", false, now); err != nil {
					return err
				}
				rep.Failed++
				continue
			}
			r, err := s.attempt(ctx, tx, w, e)
			if err != nil {
				return err
			}
			switch r.Status {
			case StatusSent:
				rep.Sent++
			case StatusFailed:
				rep.Failed++
			default:
				rep.Requeued++
			}
		}
		return nil
	})
	return rep, err
}

// noticeLocked queues a forgectl notice to a worker inside a held mailbox lock.
// The next flush delivers it. Notices to forgectl itself are dropped.
func (s *Service) noticeLocked(tx *Tx, to, body string, now time.Time) error {
	if to == SystemSender {
		return nil
	}
	clean, err := CleanBody(body)
	if err != nil {
		return fmt.Errorf("forgectl notice: %w", err)
	}
	return tx.Enqueue(Message{V: 1, ID: newID(), From: SystemSender, To: to, Body: clean, Priority: PriorityNext, CreatedAt: now}, now)
}

// Watch subscribes watcher to one notice when target next goes idle.
func (s *Service) Watch(target, watcher string) error {
	if _, err := s.worker(watcher); err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	if err := ValidateName(target); err != nil {
		return err
	}
	return s.Roster.Update(target, func(w *Worker) error {
		for _, name := range w.Watchers {
			if name == watcher {
				return nil
			}
		}
		w.Watchers = append(w.Watchers, watcher)
		return nil
	})
}

// Event is one turn boundary a harness hook reported.
type Event struct {
	Worker   string
	State    WorkerState
	ThreadID string
}

// ApplyEvent records a worker's new state (and a codex thread id), sends the
// one-shot idle notices, and flushes, since an idle worker can unblock pane
// deliveries. It returns the watchers it notified.
func (s *Service) ApplyEvent(ctx context.Context, ev Event) ([]string, error) {
	if err := ValidateName(ev.Worker); err != nil {
		return nil, err
	}
	if ev.ThreadID != "" && !threadPattern.MatchString(ev.ThreadID) {
		return nil, fmt.Errorf("thread id %s has unexpected characters", quoteTrunc(ev.ThreadID))
	}
	now := s.now()
	var watchers []string
	err := s.Roster.Update(ev.Worker, func(w *Worker) error {
		w.State = ev.State
		w.StateAt = now
		if ev.ThreadID != "" && w.Harness == HarnessCodex {
			w.ThreadID = ev.ThreadID
		}
		if ev.State == StateIdle {
			watchers = w.Watchers
			w.Watchers = nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var notified []string
	for _, name := range watchers {
		body := fmt.Sprintf("%s finished its turn and is idle.", ev.Worker)
		if _, err := s.Send(ctx, SystemSender, name, body, PriorityNext); err == nil {
			notified = append(notified, name)
		}
	}
	if ev.State == StateIdle {
		if _, err := s.Flush(ctx); err != nil {
			return notified, err
		}
	}
	return notified, nil
}

// Messages returns the folded mailbox, filtered by keep (nil keeps all).
func (s *Service) Messages(keep func(Entry) bool) ([]Entry, error) {
	var out []Entry
	err := s.Box.Locked(func(tx *Tx) error {
		entries, err := tx.Load()
		if err != nil {
			return err
		}
		for _, e := range entries {
			if keep == nil || keep(e) {
				out = append(out, e)
			}
		}
		return nil
	})
	return out, err
}
