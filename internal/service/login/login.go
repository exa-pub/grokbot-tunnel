// Package login signs the user in through the browser, like the desktop's
// deep-control login, and saves the tokens once the profile confirms them.
package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/oauth"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/instancelock"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

const profileTimeout = 20 * time.Second

// Options configure Run.
type Options struct {
	NoBrowser bool   // only print the login URL
	TeamID    string // selected team, overrides the one chosen in the browser
}

// Validate checks the options without touching the system.
func (o Options) Validate() error {
	if o.TeamID == "" {
		return nil
	}
	if n, err := strconv.ParseInt(o.TeamID, 10, 32); err != nil || n <= 0 {
		return errors.New("team must be a positive integer")
	}
	return nil
}

// Login signs the user in against one backend.
type Login struct {
	Store   *staterepo.Store
	HTTP    *http.Client // login polling
	Base    string       // backend URL
	Website string       // site with the login page
	// Profile fetches the user's profile with identity's tokens and returns
	// the identity as it stands afterwards: tokens possibly refreshed, Email
	// and AuthID set.
	Profile func(ctx context.Context, identity entity.Identity) (entity.Identity, error)
}

// Run signs the user in, saves the tokens and verifies them by fetching the
// profile. Progress is printed to out.
func (l Login) Run(ctx context.Context, opts Options, out io.Writer) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	pending := oauth.NewLogin(l.Website)
	fmt.Fprintln(out, "Open this URL to sign in:\n"+pending.URL)
	if !opts.NoBrowser {
		openBrowser(ctx, pending.URL)
	}
	identity, err := oauth.Poll(ctx, l.HTTP, l.Base, pending)
	if err != nil {
		return err
	}
	if opts.TeamID != "" {
		identity.TeamID = opts.TeamID
	}
	var previous entity.Identity
	if l.Store.Load("auth", &previous) != nil {
		previous = entity.Identity{}
	}
	if err = saveLogin(ctx, l.Store, identity); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	if identity, err = l.Profile(ctx, identity); err != nil {
		return fmt.Errorf("tokens saved; profile verification failed: %w", err)
	}
	unlock, err := l.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err = l.Store.Save("auth", identity); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in as %s\n", identity.Email)
	warnStaleConnect(l.Store, previous, identity, out)
	return nil
}

// warnStaleConnect tells the user that a running connect keeps the account it
// started with: it holds the old tokens and leaves the new login alone.
func warnStaleConnect(store *staterepo.Store, previous, current entity.Identity, out io.Writer) {
	before := previous.AccountScope()
	if before == "" || before == current.AccountScope() {
		return
	}
	h, held, err := instancelock.Probe(store.Dir, "connect")
	if err != nil || !held {
		return
	}
	account := previous.Email
	if account == "" {
		account = "the previous account"
	}
	fmt.Fprintf(out, "Note: grokbot-tunnel connect (pid %d) keeps serving %s until it is restarted\n", h.PID, account)
}

// saveLogin replaces the saved identity. It holds the store lock so that a
// concurrent token refresh in another process cannot overwrite the new login.
func saveLogin(ctx context.Context, store *staterepo.Store, identity entity.Identity) error {
	unlock, err := store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err = store.Save("auth", identity); err != nil {
		return err
	}
	// Computer credentials belong to the previous account.
	return store.Save("computer", entity.Credential{})
}

// openBrowser opens url in the desktop browser without waiting for it.
func openBrowser(ctx context.Context, url string) {
	opener := exec.CommandContext(ctx, "xdg-open", url) //nolint:gosec // url is our own login page
	if err := opener.Start(); err != nil {
		log.Printf("browser: %v", err)
		return
	}
	go func() {
		if err := opener.Wait(); err != nil {
			log.Printf("browser: %v", err)
		}
	}()
}
