// Package terminalfile writes the output files of background commands,
// <root>/terminals/<n>.txt, like the desktop's terminal logger (Fhe): a
// metadata header, rewritten in place every 5 s and when the status changes,
// the raw output and, once the command exits, a footer.
//
//	---
//	pid: 42
//	cwd: "/workspace"
//	command: "make"
//	status: running
//	started_at: 2026-01-01T00:00:00.000Z
//	running_for_ms: 5004
//	---
//	<output>
//	---
//	exit_code: 0
//	elapsed_ms: 61234
//	ended_at: 2026-01-01T00:01:01.234Z
//	---
package terminalfile

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Dir is the directory of terminal files below the root.
const Dir = "terminals"

// The statuses of a command.
const (
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Aborted   = "aborted"
)

// MaxStream is how many bytes of each output stream a terminal file keeps,
// like the desktop's 1 MiB; a marker (gN) replaces the rest.
const MaxStream = 1 << 20

const (
	timestampFormat = "2006-01-02T15:04:05.000Z"
	// refresh is how often the header is rewritten (GVe).
	refresh = 5 * time.Second
)

// Command describes the command a terminal file belongs to; PID 0 is left
// out of the header.
type Command struct {
	PID     int
	Cwd     string
	Command string
	Started time.Time
}

// File is a terminal file being written. Output goes to the underlying file at
// its offset, for example from the command itself; the header is rewritten in
// place.
type File struct {
	f   *os.File
	cmd Command

	mu     sync.Mutex
	status string
	ended  bool
	stop   chan struct{}
}

// StatusField is the status line without its newline, padded like the
// desktop's so it can be rewritten in place.
func StatusField(status string) string { return "status: " + padEnd(status) }

// Header returns the header of cmd with status, as of now (hN).
func Header(cmd Command, status string, now time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	if cmd.PID != 0 {
		fmt.Fprintf(&b, "pid: %d\n", cmd.PID)
	}
	fmt.Fprintf(&b, "cwd: %s\n", quote(cmd.Cwd))
	fmt.Fprintf(&b, "command: %s\n", quote(cmd.Command))
	b.WriteString(StatusField(status) + "\n")
	fmt.Fprintf(&b, "started_at: %s\n", cmd.Started.UTC().Format(timestampFormat))
	fmt.Fprintf(&b, "running_for_ms: %s\n", padEnd(fmt.Sprint(now.Sub(cmd.Started).Milliseconds())))
	b.WriteString("---\n")
	return b.String()
}

// Start writes the header of cmd and initial output to f, a new or truncated
// file, and keeps the header up to date until Exit.
func Start(f *os.File, cmd Command, initial []byte) (*File, error) {
	t := &File{f: f, cmd: cmd, status: Running, stop: make(chan struct{})}
	if _, err := f.Write(append([]byte(Header(cmd, Running, time.Now())), initial...)); err != nil {
		return nil, err
	}
	go t.refresh()
	return t, nil
}

// Stream returns the writer of output stream name, "stdout" or "stderr", of
// which written bytes are already in the file, or were trimmed already. It
// keeps MaxStream bytes: the write that would exceed them is replaced by a
// marker and the rest is dropped, as on the desktop. Writes after Exit are
// dropped. Writes never fail, so the command never blocks on its output.
func (t *File) Stream(name string, written int, trimmed bool) io.Writer {
	return &stream{t: t, name: name, written: written, trimmed: trimmed}
}

type stream struct {
	t       *File
	name    string
	written int
	trimmed bool
}

func (s *stream) Write(b []byte) (int, error) {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	if s.t.ended || s.trimmed {
		return len(b), nil
	}
	out := b
	if s.written+len(b) > MaxStream {
		s.trimmed = true
		out = []byte(truncated(s.name))
	}
	s.written += len(b)
	if _, err := s.t.f.Write(out); err != nil {
		log.Printf("terminal file: %v", err)
	}
	return len(b), nil
}

// Exit writes the footer for a command that ended with state, aborted by us
// or not, sets the final status and closes the file. Like the desktop, the
// status is aborted for an aborted command, else succeeded for exit code 0,
// else failed; a command killed by a signal has exit code unknown.
func (t *File) Exit(state *os.ProcessState, aborted bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.end()
	defer func() {
		if err := t.f.Close(); err != nil {
			log.Printf("terminal file: %v", err)
		}
	}()
	code := "unknown"
	if state != nil {
		if ws, ok := state.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
			code = fmt.Sprint(state.ExitCode())
		}
	}
	switch {
	case aborted:
		t.status = Aborted
	case code == "0":
		t.status = Succeeded
	default:
		t.status = Failed
	}
	now := time.Now()
	footer := fmt.Sprintf("\n---\nexit_code: %s\nelapsed_ms: %d\nended_at: %s\n---\n",
		code, now.Sub(t.cmd.Started).Milliseconds(), now.UTC().Format(timestampFormat))
	if _, err := t.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := t.f.WriteString(footer); err != nil {
		return err
	}
	return t.writeHeader(now)
}

// Stop stops writing to a file whose command never ran; the caller closes it.
func (t *File) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.end()
}

func (t *File) end() {
	if !t.ended {
		t.ended = true
		close(t.stop)
	}
}

// refresh rewrites the header every refresh until Exit.
func (t *File) refresh() {
	tick := time.NewTicker(refresh)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-tick.C:
			t.mu.Lock()
			if !t.ended {
				if err := t.writeHeader(now); err != nil {
					log.Printf("terminal file: %v", err)
				}
			}
			t.mu.Unlock()
		}
	}
}

// writeHeader rewrites the header at the start of the file. Like the desktop,
// a running time beyond the padding, after 11 days, makes it longer than the
// one it overwrites.
func (t *File) writeHeader(now time.Time) error {
	_, err := t.f.WriteAt([]byte(Header(t.cmd, t.status, now)), 0)
	return err
}

// quote quotes s like the desktop's SN: only backslash, double quote,
// newline, carriage return and tab are escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

// truncated is the desktop's marker for stream name cut at MaxStream (gN).
func truncated(name string) string {
	return fmt.Sprintf("\n[%s truncated by cursor: the command wrote more than %d characters to %s; "+
		"only the first %d were captured and the rest was dropped from this file]\n", name, MaxStream, name, MaxStream)
}

// padEnd pads s to the desktop's field width of 9.
func padEnd(s string) string { return fmt.Sprintf("%-9s", s) }
