// Package terminalfile writes and reads the output files of shell commands,
// <root>/terminals/<n>.txt, in the desktop's format: a metadata header, the
// raw output and, once the command exits, a footer.
//
//	---
//	pid: 42
//	cwd: "/workspace"
//	command: "make"
//	status: running
//	started_at: 2026-01-01T00:00:00.000Z
//	running_for_ms: 0
//	---
//	<output>
//	---
//	exit_code: 0
//	ended_at: 2026-01-01T00:00:01.000Z
//	---
package terminalfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// Dir is the directory of terminal files below the root.
const Dir = "terminals"

const (
	timestampFormat = "2006-01-02T15:04:05.000Z"
	envelopeStart   = "---\n"
	envelopeEnd     = "\n---\n"
	exitMarker      = "\n---\nexit_code: "
	errorMarker     = "\n---\nerror: "
)

// StatusField is the status line without its newline, padded so it can be
// rewritten in place.
func StatusField(status string) string { return fmt.Sprintf("status: %-9s", status) }

// Header returns the header of a command started at started; pid 0 is left
// out.
func Header(cwd, command string, pid int, started time.Time) string {
	return HeaderAt(cwd, command, pid, started, 0)
}

// HeaderAt is Header for a command that has been running for runningFor.
func HeaderAt(cwd, command string, pid int, started time.Time, runningFor time.Duration) string {
	var b strings.Builder
	b.WriteString(envelopeStart)
	if pid > 0 {
		fmt.Fprintf(&b, "pid: %d\n", pid)
	}
	fmt.Fprintf(&b, "cwd: %s\n", strconv.Quote(cwd))
	fmt.Fprintf(&b, "command: %s\n", strconv.Quote(command))
	b.WriteString(StatusField("running") + "\n")
	fmt.Fprintf(&b, "started_at: %s\n", started.UTC().Format(timestampFormat))
	fmt.Fprintf(&b, "running_for_ms: %-9d\n", runningFor.Milliseconds())
	b.WriteString("---\n")
	return b.String()
}

// WriteHeader writes the header, followed by note, and returns the offset of
// its status for AppendExit. The quoted cwd and command contain no newlines,
// so the status line is found unambiguously, however long the header is.
func WriteHeader(w io.Writer, cwd, command string, pid int, note string) (int64, error) {
	now := time.Now()
	return WriteHeaderAt(w, cwd, command, pid, now, now, note)
}

// WriteHeaderAt is WriteHeader for a command started at started, as of now.
func WriteHeaderAt(w io.Writer, cwd, command string, pid int, started, now time.Time, note string) (int64, error) {
	header := HeaderAt(cwd, command, pid, started, now.Sub(started))
	statusAt := strings.Index(header, "\n"+StatusField("running")+"\n") + 1
	_, err := io.WriteString(w, header+note)
	return int64(statusAt), err
}

// AppendExit adds the footer with code and updates the status in the header
// at statusAt. The footer is always written: only Go writes it, and output
// that looks like a footer must not stand in for the real one.
func AppendExit(f *os.File, code int, statusAt int64) error {
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	footer := fmt.Sprintf("%s%d\nended_at: %s%s", exitMarker, code, time.Now().UTC().Format(timestampFormat), envelopeEnd)
	if _, err := f.WriteString(footer); err != nil {
		return err
	}
	status := "completed"
	if code != 0 {
		status = "failed"
	}
	_, err := f.WriteAt([]byte(StatusField(status)), statusAt)
	return err
}

// Ended reports whether b ends with a footer.
func Ended(b []byte) bool {
	i := max(bytes.LastIndex(b, []byte(exitMarker)), bytes.LastIndex(b, []byte(errorMarker)))
	if i < 0 || !bytes.HasSuffix(b, []byte(envelopeEnd)) {
		return false
	}
	tail := string(b[i:])
	return strings.Contains(tail, "ended_at: ") && (strings.Contains(tail, "exit_code: ") || strings.Contains(tail, "error: "))
}

// HeaderEnd returns the offset just past the header of a terminal file, or
// -1 if b has none.
func HeaderEnd(b []byte) int {
	if !bytes.HasPrefix(b, []byte(envelopeStart)) {
		return -1
	}
	i := bytes.Index(b[len(envelopeStart):], []byte(envelopeEnd))
	if i < 0 {
		return -1
	}
	return len(envelopeStart) + i + len(envelopeEnd)
}

// Decode decodes the output of a terminal file with hint while keeping the
// UTF-8 header and footer intact.
func Decode(b []byte, hint string) (string, error) {
	start := max(HeaderEnd(b), 0)
	end := len(b)
	if i := max(bytes.LastIndex(b, []byte(exitMarker)), bytes.LastIndex(b, []byte(errorMarker))); i >= start && Ended(b) {
		end = i
	}
	body, _, err := textcodec.Decode(b[start:end], hint)
	if err != nil {
		return "", err
	}
	// The envelope is UTF-8 when written by Go, but the file may have been
	// edited.
	return strings.ToValidUTF8(string(b[:start]), "�") + body + strings.ToValidUTF8(string(b[end:]), "�"), nil
}
