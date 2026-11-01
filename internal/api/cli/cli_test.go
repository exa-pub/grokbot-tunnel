package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
	"github.com/exa-pub/grokbot-tunnel/internal/service/selfupdate"
)

// recorder is an App that records the options it gets.
type recorder struct {
	connect ConnectOptions
	install install.Options
}

func (r *recorder) Login(context.Context, LoginOptions, io.Writer) error { return nil }

func (r *recorder) Connect(_ context.Context, opts ConnectOptions) error {
	r.connect = opts
	return nil
}

func (r *recorder) Install(_ context.Context, opts install.Options, _ io.Writer) error {
	r.install = opts
	return nil
}

func (r *recorder) Uninstall(context.Context, io.Writer) error { return nil }

func (r *recorder) Update(context.Context, selfupdate.Options, io.Writer) error { return nil }

// --label is trimmed and cut to 100 code points, like the desktop's label.
func TestLabelNormalized(t *testing.T) {
	long := strings.Repeat("ж", 120)
	for _, args := range [][]string{
		{"connect", "--label", "  my box "},
		{"install", "--permission", "always", "--label", "  my box "},
		{"connect", "--label", long},
	} {
		r := &recorder{}
		if err := Execute(context.Background(), args, r); err != nil {
			t.Fatal(err)
		}
		want := "my box"
		if args[len(args)-1] == long {
			want = strings.Repeat("ж", 100)
		}
		if got := r.connect.Label + r.install.Label; got != want {
			t.Errorf("%v: got %q, want %q", args, got, want)
		}
	}
}

func TestExitCode(t *testing.T) {
	for err, want := range map[error]int{
		errors.New("boom"):                           1,
		entity.ErrNotLoggedIn:                        78,
		fmt.Errorf("register: %w", entity.ErrLogout): 78,
	} {
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}
