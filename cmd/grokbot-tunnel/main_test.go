package main

import (
	"context"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// Each stop signal cancels the context instead of killing the process, so
// that connect withdraws the computer before it exits.
func TestStopSignalsCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%v did not cancel", sig)
		}
		stop()
	}
}
