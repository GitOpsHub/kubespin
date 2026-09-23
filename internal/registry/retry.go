package registry

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// RetryPolicy bounds how hard a Postgres call tries before giving up.
//
// It exists because the registry sits on the critical path of operations that
// run for tens of minutes against a database that may be remote: a single
// TCP read timeout while recording a phase transition used to abandon a
// cluster that had already been created, and a few missed lease renewals
// could kill a provision outright. Neither is a reason to stop — the write
// simply has not happened yet.
type RetryPolicy struct {
	// Attempts is the total number of tries, not the number of retries. One
	// means no retrying at all.
	Attempts int

	// Backoff is the pause before the second attempt; it doubles after each
	// one, so the default policy waits 250ms, 500ms, then 1s.
	Backoff time.Duration

	// Timeout bounds a single attempt. Without it a connection to an
	// unreachable host blocks for the operating system's TCP timeout —
	// minutes on macOS — and the retry never gets a chance to run. It never
	// extends a caller's own deadline, only shortens it.
	Timeout time.Duration
}

// DefaultRetryPolicy is applied to every Postgres registry unless
// WithRetryPolicy replaces it: four attempts over roughly 1.75s of backoff,
// each capped at 20s. The worst case is bounded well under a minute, which is
// short against the phases it protects and long enough to ride out the
// restarts and failovers a managed Postgres does routinely.
var DefaultRetryPolicy = RetryPolicy{
	Attempts: 4,
	Backoff:  250 * time.Millisecond,
	Timeout:  20 * time.Second,
}

// normalise fills in zero fields, so a partially-specified policy is usable
// rather than a busy loop or an instant give-up.
func (p RetryPolicy) normalise() RetryPolicy {
	if p.Attempts < 1 {
		p.Attempts = DefaultRetryPolicy.Attempts
	}
	if p.Backoff <= 0 {
		p.Backoff = DefaultRetryPolicy.Backoff
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultRetryPolicy.Timeout
	}
	return p
}

// retry runs fn until it succeeds, fails with something not worth retrying,
// or runs out of attempts. It reports how many attempts it took, so a caller
// whose operation is conditional (Create, UpdatePhase) can tell a genuine
// conflict from its own earlier write having landed unacknowledged.
func (p *Postgres) retry(ctx context.Context, what string, fn func(context.Context) error) (attempts int, err error) {
	policy := p.retryPolicy.normalise()
	backoff := policy.Backoff

	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, policy.Timeout)
		err = fn(attemptCtx)
		cancel()

		switch {
		case err == nil:
			return attempt, nil

		// The caller gave up (interrupted, or a parent deadline passed).
		// Retrying would ignore that.
		case ctx.Err() != nil:
			return attempt, err

		case !isTransient(err):
			return attempt, err

		case attempt == policy.Attempts:
			return attempt, err
		}

		p.log().Warn("Registry Call Failed, Retrying",
			"operation", what, "attempt", fmt.Sprintf("%d/%d", attempt, policy.Attempts), "backoff", backoff, "error", err)

		select {
		case <-ctx.Done():
			return attempt, err
		case <-time.After(backoff):
		}
		backoff *= 2
	}

	return policy.Attempts, err
}

// isTransient reports whether err is the kind of failure that a later attempt
// could plausibly succeed through: the database was unreachable, the
// connection died under us, or Postgres itself said "not right now".
//
// Everything else — a constraint violation, a syntax error, a version
// conflict — means the call will fail identically forever, and retrying only
// delays the report.
func isTransient(err error) bool {
	if err == nil {
		return false
	}

	// A dead pooled connection. database/sql retries this itself only when it
	// can prove the statement never ran; once it surfaces here it is ours.
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}

	// The connection dropped mid-statement.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// The network-level failures behind "operation timed out", "connection
	// refused" and "connection reset by peer".
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}
	for _, errno := range []syscall.Errno{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED,
		syscall.EPIPE, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}

	// Postgres said so itself. The 08 class is every connection exception;
	// the rest are the server declining this particular moment, not this
	// particular statement.
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"53300", // too_many_connections
			"53400", // configuration_limit_exceeded
			"55006", // object_in_use
			"57P01", // admin_shutdown
			"57P02", // crash_shutdown
			"57P03", // cannot_connect_now — a server still starting up
			"58000", // system_error
			"58030": // io_error
			return true
		}
		if len(pgErr.Code) >= 2 && pgErr.Code[:2] == "08" {
			return true
		}
		return false
	}

	// A pgconn-level connect failure that carries no PgError of its own.
	_, isConnectError := errors.AsType[*pgconn.ConnectError](err)
	return isConnectError
}
