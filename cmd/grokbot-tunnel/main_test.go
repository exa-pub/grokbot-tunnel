package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Each stop signal cancels the context instead of killing the process, so
// that connect withdraws the computer before it exits.
func TestStopSignalsCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if sig == syscall.SIGHUP && signal.Ignored(sig) {
			continue // under nohup; see TestStopSignalsKeepIgnoredHangup
		}
		ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
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

// helperEnv makes the test binary run TestStopSignalsHelper.
const helperEnv = "GROKBOT_TUNNEL_SIGNAL_HELPER"

// TestStopSignalsHelper is the child of TestStopSignalsKeepIgnoredHangup: it
// waits for a stop signal like main and reports it.
func TestStopSignalsHelper(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process")
	}
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	defer stop()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("stopped")
}

// A program started with SIGHUP ignored, as by nohup, keeps ignoring it:
// handling it would stop connect when the terminal goes away. Otherwise
// SIGHUP stops it cleanly.
func TestStopSignalsKeepIgnoredHangup(t *testing.T) {
	for _, tc := range []struct {
		name, trap string
		survives   bool
	}{
		{"ignored", `trap '' HUP; `, true},
		{"default", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", tc.trap+`exec "$0" -test.run='^TestStopSignalsHelper$'`, os.Args[0])
			cmd.Env = append(os.Environ(), helperEnv+"=1")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := make(chan string, 16)
			go func() {
				sc := bufio.NewScanner(out)
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()
			waitLine := func(want string, d time.Duration) bool {
				timer := time.After(d)
				for {
					select {
					case l, ok := <-lines:
						if !ok {
							return false
						}
						if strings.TrimSpace(l) == want {
							return true
						}
					case <-timer:
						return false
					}
				}
			}
			defer cmd.Process.Kill()
			if !waitLine("ready", 10*time.Second) {
				t.Fatal("helper did not start")
			}
			if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
				t.Fatal(err)
			}
			stopped := waitLine("stopped", time.Second)
			if stopped == tc.survives {
				t.Fatalf("stopped on SIGHUP: %v", stopped)
			}
			if tc.survives {
				// It still stops cleanly on SIGTERM.
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				if !waitLine("stopped", 5*time.Second) {
					t.Fatal("did not stop on SIGTERM")
				}
			}
			for range lines {
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
