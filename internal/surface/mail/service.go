package mail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Service sends and flushes messages for one coordinator ledger.
type Service struct {
	Box         Mailbox
	Roster      Roster
	Adapters    map[Harness]Adapter
	Policy      Policy
	ReplyBinary string
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
	Sent, Requeued, Failed, Expired, Unconfirmed int
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
	if err := ValidateName(name); err != nil {
		return Worker{}, err
	}
	return s.Roster.Get(name)
}

// Send checks policy, appends the message, and attempts delivery once. A
// transient failure leaves it queued for the next flush.
func (service *Service) Send(ctx context.Context, from, to, body string, pri Priority) (SendResult, error) {
	return service.send(ctx, from, to, body, pri, false)
}

func (service *Service) Queue(ctx context.Context, from, to, body string, pri Priority) (SendResult, error) {
	return service.send(ctx, from, to, body, pri, true)
}

func (service *Service) send(ctx context.Context, from, to, body string, pri Priority, queueOnly bool) (SendResult, error) {
	if err := ValidateReplyBinary(service.ReplyBinary); err != nil {
		return SendResult{}, err
	}
	sender, err := service.worker(from)
	if err != nil {
		return SendResult{}, fmt.Errorf("sender: %w", err)
	}
	recip, err := service.worker(to)
	if err != nil {
		return SendResult{}, fmt.Errorf("recipient: %w", err)
	}
	if sender.Name == recip.Name {
		return SendResult{}, errors.New("a worker cannot message itself")
	}
	if (sender.RunID != "" && !threadPattern.MatchString(sender.RunID)) || (recip.RunID != "" && !threadPattern.MatchString(recip.RunID)) {
		return SendResult{}, errors.New("sender or recipient has an invalid run identity")
	}
	clean, err := CleanBody(body)
	if err != nil {
		return SendResult{}, err
	}
	if pri == "" {
		pri = PriorityNext
	}
	now := service.now()
	msg := Message{V: 1, ID: newID(), From: sender.Name, To: recip.Name, FromRunID: sender.RunID, ToRunID: recip.RunID, Body: clean, Priority: pri, CreatedAt: now, ReplyBinary: service.ReplyBinary}

	var res SendResult
	err = service.Box.Locked(func(tx *Tx) error {
		history, err := tx.Load()
		if err != nil {
			return err
		}
		if err := service.Policy.Check(sender, recip, clean, history, now); err != nil {
			return err
		}
		if err := tx.Enqueue(msg, now); err != nil {
			return err
		}
		if queueOnly {
			res = SendResult{ID: msg.ID, Status: StatusQueued}
			return nil
		}
		res, err = service.attempt(ctx, tx, recip, Entry{Msg: msg, Status: StatusQueued})
		return err
	})
	return res, err
}

// attempt tries one delivery and records the outcome.
func (s *Service) attempt(ctx context.Context, tx *Tx, w Worker, e Entry) (SendResult, error) {
	id := e.Msg.ID
	if e.Msg.ToRunID != "" && e.Msg.ToRunID != w.RunID {
		const detail = "recipient run changed; queued message was not delivered to the replacement"
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, false, s.now())
	}
	if err := s.nativeReplyAttemptAllowed(tx, e.Msg); err != nil {
		detail := oneLine([]byte(err.Error()), 300)
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, false, s.now())
	}
	if err := ValidateReplyBinary(e.Msg.ReplyBinary); err != nil {
		detail := oneLine([]byte(err.Error()), 300)
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, true, s.now())
	}
	ad, ok := s.Adapters[w.Harness]
	if !ok || ad == nil {
		detail := fmt.Sprintf("no adapter for harness %s", quoteTrunc(string(w.Harness)))
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, true, s.now())
	}
	started := s.now()
	if err := tx.Mark(id, StatusUnconfirmed, "delivery attempt in progress", true, started); err != nil {
		return SendResult{ID: id, Status: StatusUnconfirmed, Detail: "delivery attempt could not be recorded"}, err
	}
	detail, err := ad.Deliver(ctx, w, e.Msg, Render(e.Msg))
	now := s.now()
	switch {
	case err == nil:
		return SendResult{ID: id, Status: StatusSent, Detail: detail}, tx.Mark(id, StatusSent, detail, false, now)
	case IsRetryable(err):
		detail = oneLine([]byte(err.Error()), 300)
		st := StatusQueued
		if s.Policy.MaxAttempts > 0 && e.Attempts+1 >= s.Policy.MaxAttempts {
			st = StatusFailed
			detail = fmt.Sprintf("gave up after %d attempts: %s", e.Attempts+1, detail)
		}
		return SendResult{ID: id, Status: st, Detail: detail}, tx.Mark(id, st, detail, false, now)
	case IsUnconfirmed(err):
		detail = oneLine([]byte(err.Error()), 300)
		return SendResult{ID: id, Status: StatusUnconfirmed, Detail: detail}, tx.Mark(id, StatusUnconfirmed, detail, false, now)
	default:
		detail = oneLine([]byte(err.Error()), 300)
		return SendResult{ID: id, Status: StatusFailed, Detail: detail}, tx.Mark(id, StatusFailed, detail, false, now)
	}
}

func (s *Service) reconcile(ctx context.Context, tx *Tx, worker Worker, entry Entry) (SendResult, error) {
	if entry.Msg.ToRunID != "" && entry.Msg.ToRunID != worker.RunID {
		return s.retainUnconfirmed(tx, entry, "recipient run changed; original delivery remains unconfirmed")
	}
	adapter, ok := s.Adapters[worker.Harness]
	if !ok || adapter == nil {
		detail := fmt.Sprintf("no adapter for harness %s", quoteTrunc(string(worker.Harness)))
		return s.retainUnconfirmed(tx, entry, detailIfMissing(entry, detail))
	}
	receiptAdapter, ok := adapter.(ReceiptAdapter)
	if !ok {
		detail := fmt.Sprintf("no receipt adapter for harness %s", quoteTrunc(string(worker.Harness)))
		return s.retainUnconfirmed(tx, entry, detailIfMissing(entry, detail))
	}
	detail, accepted, err := receiptAdapter.Receipt(ctx, worker, entry.Msg)
	if err != nil {
		detail = oneLine([]byte(err.Error()), 300)
		return s.retainUnconfirmed(tx, entry, detail)
	}
	if !accepted {
		if detail == "" {
			detail = "receipt did not confirm harness acceptance"
		}
		return s.retainUnconfirmed(tx, entry, detail)
	}
	return SendResult{ID: entry.Msg.ID, Status: StatusSent, Detail: detail}, tx.Mark(entry.Msg.ID, StatusSent, detail, false, s.now())
}

func (s *Service) retainUnconfirmed(tx *Tx, entry Entry, detail string) (SendResult, error) {
	if detail == "" {
		detail = entry.Detail
	}
	result := SendResult{ID: entry.Msg.ID, Status: StatusUnconfirmed, Detail: detail}
	if detail == entry.Detail {
		return result, nil
	}
	return result, tx.Mark(entry.Msg.ID, StatusUnconfirmed, detail, false, s.now())
}

func detailIfMissing(entry Entry, fallback string) string {
	if entry.Detail != "" {
		return entry.Detail
	}
	return fallback
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
			if e.Status == StatusUnconfirmed {
				recipient, err := s.workerForTx(tx, e.Msg.To)
				if err != nil && !errors.Is(err, ErrUnknownWorker) {
					return fmt.Errorf("roster: %w", err)
				}
				if err != nil {
					detail := "recipient is no longer in the roster"
					if _, err := s.retainUnconfirmed(tx, e, detail); err != nil {
						return err
					}
					rep.Unconfirmed++
					continue
				}
				receiptResult, err := s.reconcile(ctx, tx, recipient, e)
				if err != nil {
					return err
				}
				switch receiptResult.Status {
				case StatusSent:
					rep.Sent++
				default:
					rep.Unconfirmed++
				}
				continue
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
			w, err := s.workerForTx(tx, e.Msg.To)
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
			case StatusUnconfirmed:
				rep.Unconfirmed++
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
	if err := ValidateName(to); err != nil {
		return fmt.Errorf("forgectl notice: %w", err)
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

var ErrRunIDMismatch = errors.New("worker event run id does not match the registered run")

// Event is one turn boundary a harness hook reported.
type Event struct {
	Worker   string
	State    WorkerState
	ThreadID string
	RunID    string
}

// ApplyEvent records a worker's new state (and a codex thread id), queues
// the one-shot idle notices, and flushes, since an idle worker can unblock
// pane deliveries and the flush is what delivers the notices. It returns the
// watchers it queued a notice for.
//
// A notice is forgectl's own message, so it skips the sender policy: a
// burst of workers going idle, or one worker finishing two watched turns
// inside the dedupe window, must not drop a notice someone asked for.
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
		if w.RunID != "" && ev.RunID != w.RunID {
			return fmt.Errorf("worker event does not match the registered run: %w", ErrRunIDMismatch)
		}
		// A Codex worker's thread is fixed by its first turn. A notify from
		// another thread (an internal one Codex starts beside it) says
		// nothing about the worker, and taking it would point sends at the
		// wrong thread and call a busy worker idle.
		if w.Harness == HarnessCodex && w.ThreadID != "" && ev.ThreadID != "" && ev.ThreadID != w.ThreadID {
			return fmt.Errorf("codex notify for thread %s, not this worker's: %w", quoteTrunc(ev.ThreadID), ErrNotTurnEvent)
		}
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
	if len(watchers) > 0 {
		body := fmt.Sprintf("%s finished its turn and is idle.", ev.Worker)
		// Each notice is appended on its own, so a failure part way leaves
		// the earlier ones queued: give back only the watchers whose notice
		// did not land, or the next idle would notify the others twice.
		var failed []string
		var firstErr error
		lockErr := s.Box.Locked(func(tx *Tx) error {
			for _, name := range watchers {
				if err := s.noticeLocked(tx, name, body, now); err != nil {
					failed = append(failed, name)
					if firstErr == nil {
						firstErr = err
					}
				}
			}
			return nil
		})
		if lockErr != nil {
			failed, firstErr = watchers, lockErr
		}
		if len(failed) > 0 {
			// A Watch that ran in between may have re-added one already.
			_ = s.Roster.Update(ev.Worker, func(w *Worker) error {
				for _, name := range failed {
					if !slices.Contains(w.Watchers, name) {
						w.Watchers = append(w.Watchers, name)
					}
				}
				return nil
			})
			return nil, fmt.Errorf("queue idle notices: %w", firstErr)
		}
	}
	if ev.State == StateIdle {
		if _, err := s.Flush(ctx); err != nil {
			return watchers, err
		}
	}
	return watchers, nil
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
