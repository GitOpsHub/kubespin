package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// A long wait logs that it is still going, and stops logging once stopped.
func TestLogWhileWaiting_LogsUntilStopped(t *testing.T) {
	previous := waitingLogInterval
	waitingLogInterval = 10 * time.Millisecond
	t.Cleanup(func() { waitingLogInterval = previous })

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	stop := logWhileWaiting(context.Background(), logger, "eks-spot", "control plane")
	time.Sleep(35 * time.Millisecond)
	stop()
	logged := strings.Count(buf.String(), "Still Waiting")
	if logged == 0 {
		t.Fatal("expected at least one Still Waiting line")
	}

	time.Sleep(30 * time.Millisecond)
	if after := strings.Count(buf.String(), "Still Waiting"); after != logged {
		t.Errorf("logged %d more lines after stop", after-logged)
	}
	if !strings.Contains(buf.String(), "for=\"control plane\"") {
		t.Errorf("line does not say what it waits for: %q", buf.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
