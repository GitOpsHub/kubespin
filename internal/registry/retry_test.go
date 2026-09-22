package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// timeoutError is the shape a TCP read timeout arrives in — the exact failure
// that abandoned a created-but-unrecorded GKE cluster mid-apply.
type timeoutError struct{}

func (timeoutError) Error() string { return "read tcp 10.0.0.1:5432: i/o timeout" }
func (timeoutError) Timeout() bool { return true }
func (timeoutError) Temporary() bool {
	return true
}

func TestIsTransient(t *testing.T) {
	var netErr net.Error = timeoutError{}

	transient := map[string]error{
		"read timeout":      netErr,
		"wrapped timeout":   fmt.Errorf("scanning record: %w", netErr),
		"connection reset":  syscall.ECONNRESET,
		"connection refuse": syscall.ECONNREFUSED,
		"broken pipe":       syscall.EPIPE,
		"host unreachable":  syscall.EHOSTUNREACH,
		"eof mid-statement": io.ErrUnexpectedEOF,
		"admin shutdown":    &pgconn.PgError{Code: "57P01"},
		"starting up":       &pgconn.PgError{Code: "57P03"},
		"too many conns":    &pgconn.PgError{Code: "53300"},
		"deadlock":          &pgconn.PgError{Code: "40P01"},
		"connection class":  &pgconn.PgError{Code: "08006"},
	}
	for name, err := range transient {
		if !isTransient(err) {
			t.Errorf("%s: isTransient = false, want true", name)
		}
	}

	permanent := map[string]error{
		"nil":                nil,
		"foreign key":        &pgconn.PgError{Code: "23503"},
		"unique violation":   &pgconn.PgError{Code: "23505"},
		"syntax error":       &pgconn.PgError{Code: "42601"},
		"undefined table":    &pgconn.PgError{Code: "42P01"},
		"registry sentinel":  ErrVersionConflict,
		"context cancelled":  context.Canceled,
		"an ordinary string": errors.New("something else"),
	}
	for name, err := range permanent {
		if isTransient(err) {
			t.Errorf("%s: isTransient = true, want false — retrying it only delays the report", name)
		}
	}
}

// retryTestPostgres builds a Postgres with no database behind it. Every test
// here drives p.retry directly, which never touches p.db.
func retryTestPostgres(policy RetryPolicy) *Postgres {
	return &Postgres{now: time.Now, retryPolicy: policy}
}

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	p := retryTestPostgres(RetryPolicy{Attempts: 4, Backoff: time.Millisecond, Timeout: time.Second})

	calls := 0
	attempts, err := p.retry(context.Background(), "test", func(context.Context) error {
		calls++
		if calls < 3 {
			return timeoutError{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v, want the third attempt to succeed", err)
	}
	if attempts != 3 || calls != 3 {
		t.Errorf("attempts = %d, calls = %d, want 3 and 3", attempts, calls)
	}
}

func TestRetry_GivesUpAfterTheLastAttempt(t *testing.T) {
	p := retryTestPostgres(RetryPolicy{Attempts: 3, Backoff: time.Millisecond, Timeout: time.Second})

	calls := 0
	attempts, err := p.retry(context.Background(), "test", func(context.Context) error {
		calls++
		return timeoutError{}
	})
	if err == nil {
		t.Fatal("retry succeeded, want the last failure surfaced")
	}
	if attempts != 3 || calls != 3 {
		t.Errorf("attempts = %d, calls = %d, want exactly the configured 3", attempts, calls)
	}
}

func TestRetry_DoesNotRetryPermanentFailures(t *testing.T) {
	p := retryTestPostgres(RetryPolicy{Attempts: 5, Backoff: time.Millisecond, Timeout: time.Second})

	calls := 0
	_, err := p.retry(context.Background(), "test", func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "23503"}
	})
	if err == nil {
		t.Fatal("retry succeeded, want the foreign-key violation surfaced")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1: a constraint violation fails identically forever", calls)
	}
}

// An operator's ctrl-C must stop the work, not be retried around.
func TestRetry_StopsOnCallerCancellation(t *testing.T) {
	p := retryTestPostgres(RetryPolicy{Attempts: 5, Backoff: 10 * time.Millisecond, Timeout: time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := p.retry(ctx, "test", func(context.Context) error {
		calls++
		cancel()
		return timeoutError{}
	})
	if err == nil {
		t.Fatal("retry succeeded, want the cancellation surfaced")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1: a cancelled caller must not be retried around", calls)
	}
}

// The per-attempt timeout is what makes retrying possible at all against a
// host that black-holes packets: without it the first attempt blocks for the
// operating system's TCP timeout and no retry ever runs.
func TestRetry_BoundsEachAttempt(t *testing.T) {
	p := retryTestPostgres(RetryPolicy{Attempts: 2, Backoff: time.Millisecond, Timeout: 20 * time.Millisecond})

	var deadlineSeen bool
	_, err := p.retry(context.Background(), "test", func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			deadlineSeen = true
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !deadlineSeen {
		t.Error("the attempt context carried no deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// A caller's own shorter deadline must win over the policy's.
func TestRetry_NeverExtendsTheCallersDeadline(t *testing.T) {
	p := retryTestPostgres(RetryPolicy{Attempts: 2, Backoff: time.Millisecond, Timeout: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := p.retry(ctx, "test", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}); err == nil {
		t.Fatal("retry succeeded, want the caller's deadline to end it")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s, want the caller's 20ms deadline to have ended it", elapsed)
	}
}

func TestRetryPolicy_NormaliseFillsZeroFields(t *testing.T) {
	got := RetryPolicy{}.normalise()
	if got != DefaultRetryPolicy {
		t.Errorf("normalise() = %+v, want %+v", got, DefaultRetryPolicy)
	}

	// A deliberate single attempt is not a zero value and must survive.
	if got := (RetryPolicy{Attempts: 1}).normalise(); got.Attempts != 1 {
		t.Errorf("Attempts = %d, want the explicit 1 to be kept", got.Attempts)
	}
}
