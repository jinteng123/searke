package deploy

import (
	"context"
	"io"
	"testing"
	"time"
)

// A cancelled stream must return promptly even when a child keeps the
// pipes open: the parent dies on SIGTERM and WaitDelay bounds the rest.
func TestLocalStreamCancelBounded(t *testing.T) {
	l := &LocalExecutor{}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := l.RunSudoStreamContext(ctx, "sleep 30; echo done", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected context error")
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("cancel took %s; orphaned children must not hold the pipe", took)
	}
}
