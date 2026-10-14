package mail

import (
	"errors"
	"fmt"
	"time"
)

// Policy bounds who may message whom, how much, and for how long forgectl
// keeps trying.
type Policy struct {
	// MaxBody caps a cleaned body, in bytes.
	MaxBody int
	// PeerMessages allows worker-to-worker messages. Off by default: workers
	// message the coordinator, and the coordinator messages any worker.
	PeerMessages bool
	// PerPairPerMinute caps messages from one sender to one recipient.
	PerPairPerMinute int
	// DedupeWindow drops an identical body to the same recipient.
	DedupeWindow time.Duration
	// TTL is how long a message may stay queued before it expires.
	TTL time.Duration
	// MaxAttempts caps delivery attempts before a message fails.
	MaxAttempts int
}

// DefaultPolicy is the [surface] default.
func DefaultPolicy() Policy {
	return Policy{
		MaxBody:          32 << 10,
		PerPairPerMinute: 10,
		DedupeWindow:     30 * time.Second,
		TTL:              30 * time.Minute,
		MaxAttempts:      20,
	}
}

var (
	ErrPeerBlocked = errors.New("worker-to-worker messages are off: send to the coordinator, or set [surface] peer_messages = true")
	ErrRateLimited = errors.New("too many messages to this recipient in the last minute; batch them into one")
	ErrDuplicate   = errors.New("an identical message to this recipient was sent moments ago")
	ErrTooLarge    = errors.New("message body is over the size cap")
)

// Check decides whether a message may be sent, against the recent mailbox.
func (p Policy) Check(from, to Worker, body string, history []Entry, now time.Time) error {
	if p.MaxBody > 0 && len(body) > p.MaxBody {
		return fmt.Errorf("%w: %d bytes, cap %d", ErrTooLarge, len(body), p.MaxBody)
	}
	system := from.Name == SystemSender
	if !system && !p.PeerMessages && !from.Coordinator && !to.Coordinator {
		return ErrPeerBlocked
	}
	recent := 0
	for _, e := range history {
		if e.Msg.From != from.Name || e.Msg.To != to.Name {
			continue
		}
		age := now.Sub(e.Msg.CreatedAt)
		if age < time.Minute {
			recent++
		}
		if p.DedupeWindow > 0 && age < p.DedupeWindow && e.Msg.Body == body {
			return ErrDuplicate
		}
	}
	if p.PerPairPerMinute > 0 && recent >= p.PerPairPerMinute {
		return ErrRateLimited
	}
	return nil
}

// nextAttempt is when a queued entry is due again: 5s after the first failed
// attempt, doubling to a 2 minute ceiling.
func (p Policy) nextAttempt(e Entry) time.Time {
	if e.Attempts == 0 {
		return time.Time{}
	}
	shift := e.Attempts - 1
	if shift > 5 {
		shift = 5
	}
	d := (5 * time.Second) << shift
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return e.LastAttempt.Add(d)
}
