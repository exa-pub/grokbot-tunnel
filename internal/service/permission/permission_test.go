package permission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/tty"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

// openPTY returns the master and slave ends of a new pseudo-terminal.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pty:", err)
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	// Drain the echo so the pty never blocks on a full output buffer.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()
	return master, slave
}

// promptWriter signals every time a prompt is printed.
type promptWriter chan string

func (w promptWriter) Write(b []byte) (int, error) {
	w <- string(b)
	return len(b), nil
}

func TestTypeaheadDoesNotAnswerPrompt(t *testing.T) {
	master, slave := openPTY(t)
	out := make(promptWriter, 4)
	p := &Policy{Mode: entity.ModeAsk, Prompter: tty.Terminal{In: slave, Out: out}}
	if _, err := master.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the line discipline queue the input
	done := make(chan error, 1)
	go func() { done <- p.Allow(context.Background(), "Run x", false, false) }()
	if prompt := <-out; !strings.Contains(prompt, "Run x?") {
		t.Fatal(prompt)
	}
	if _, err := master.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("typeahead answered the prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt not answered")
	}
}

func TestQueuedPromptHonorsCancellation(t *testing.T) {
	_, slave := openPTY(t)
	out := make(promptWriter, 4)
	p := &Policy{Mode: entity.ModeAsk, Prompter: tty.Terminal{In: slave, Out: out}}
	first, stopFirst := context.WithCancel(context.Background())
	defer stopFirst()
	firstDone := make(chan error, 1)
	go func() { firstDone <- p.Allow(first, "first", false, false) }()
	<-out // the first prompt holds the terminal
	second, stopSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- p.Allow(second, "second", false, false) }()
	time.Sleep(50 * time.Millisecond)
	stopSecond()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued prompt ignored cancellation")
	}
	stopFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// fakePrompter answers every question with answer and records the questions.
type fakePrompter struct {
	answer    string
	questions []string
}

func (f *fakePrompter) Interactive() bool { return true }

func (f *fakePrompter) Ask(_ context.Context, question string) (string, error) {
	f.questions = append(f.questions, question)
	return f.answer, nil
}

// L13: a request that cannot be described (an unsupported exec) is refused
// without a prompt, as the desktop refuses it without approval; under
// "always" it is allowed and fails on its own.
func TestEmptyActionIsNotPrompted(t *testing.T) {
	f := &fakePrompter{answer: "y"}
	p := &Policy{Mode: entity.ModeAsk, Prompter: f}
	if err := p.Allow(context.Background(), "", false, false); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if len(f.questions) != 0 {
		t.Fatal(f.questions)
	}
	p.Mode = entity.ModeAlways
	if err := p.Allow(context.Background(), "", false, false); err != nil {
		t.Fatal(err)
	}
}

// L13: the prompt says what "a" does; "a" allows every later action
// without asking.
func TestPromptExplainsAllowAll(t *testing.T) {
	f := &fakePrompter{answer: "a"}
	p := &Policy{Mode: entity.ModeAsk, Prompter: f}
	if err := p.Allow(context.Background(), `Run "ls" in ""`, false, false); err != nil {
		t.Fatal(err)
	}
	if len(f.questions) != 1 || f.questions[0] != `Run "ls" in ""? [y/N, a = allow all actions for the rest of this session] ` {
		t.Fatal(f.questions)
	}
	if err := p.Allow(context.Background(), "Read x", false, false); err != nil || len(f.questions) != 1 || p.Standing() != entity.ModeAlways {
		t.Fatal(err, f.questions, p.Standing())
	}
}
