// Package tty asks questions on the user's terminal. It knows terminal input
// and output only; what to ask and what an answer means is up to the caller.
package tty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const maxAnswer = 4096

// errNoAnswer ends input without an answer: end of input or a line above
// maxAnswer bytes.
var errNoAnswer = errors.New("no answer")

// Terminal is a terminal to ask on: In is read for answers, Out (stderr if
// nil) shows the questions.
type Terminal struct {
	In  *os.File
	Out io.Writer
}

// Interactive reports whether In is a terminal the CLI may ask on. If it is
// the controlling terminal, the CLI must be in its foreground process group:
// reading or flushing from the background (connect &) stops the whole process
// with SIGTTIN or SIGTTOU. Other terminals report ENOTTY and are not subject
// to job control.
func (t Terminal) Interactive() bool {
	if t.In == nil {
		return false
	}
	fd := int(t.In.Fd())
	if !term.IsTerminal(fd) {
		return false
	}
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if errors.Is(err, unix.ENOTTY) {
		return true
	}
	return err == nil && pgrp == unix.Getpgrp()
}

// Ask shows question with its non-printable characters escaped and returns
// the first line typed after it, without the newline. Keystrokes typed before
// the question appeared are discarded so they cannot answer it. End of input
// and an overlong line give an empty answer.
func (t Terminal) Ask(ctx context.Context, question string) (string, error) {
	out := t.Out
	if out == nil {
		out = os.Stderr
	}
	if err := flushInput(t.In); err != nil {
		return "", err
	}
	fmt.Fprint(out, Printable(question))
	answer, err := readLine(ctx, t.In)
	if err != nil {
		fmt.Fprintln(out)
		if errors.Is(err, errNoAnswer) {
			return "", nil
		}
		return "", err
	}
	return answer, nil
}

// Printable escapes every non-printable rune in s, so no input can move the
// cursor, erase the line or reorder text on the user's terminal.
func Printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// flushInput discards pending terminal input; it is a no-op for non-terminals.
func flushInput(f *os.File) error {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	if err := unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH); err != nil {
		return fmt.Errorf("discard terminal input: %w", err)
	}
	return nil
}

// readLine polls canonical terminal input so cancellation does not strand a
// blocked reader. It returns the first line without the newline.
func readLine(ctx context.Context, f *os.File) (string, error) {
	fd := int(f.Fd())
	if fd < 0 || fd > math.MaxInt32 {
		return "", fmt.Errorf("invalid terminal descriptor %d", fd)
	}
	var answer []byte
	buf := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR || n == 0 {
			continue
		}
		if err != nil {
			return "", err
		}
		n, err = unix.Read(fd, buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", errNoAnswer
		}
		answer = append(answer, buf[:n]...)
		if line, _, found := strings.Cut(string(answer), "\n"); found {
			return line, nil
		}
		if len(answer) > maxAnswer {
			return "", errNoAnswer
		}
	}
}
