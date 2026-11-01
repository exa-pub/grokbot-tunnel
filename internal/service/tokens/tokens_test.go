package tokens

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

func TestRefreshRotationSingleFlight(t *testing.T) {
	var calls atomic.Int32
	token := jwt(time.Now().Add(time.Hour))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "old" {
			t.Errorf("unexpected refresh: %v", body)
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": token, "refresh_token": "new"})
	}))
	defer s.Close()
	store := &staterepo.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{AccessToken: jwt(time.Now().Add(-time.Minute)), RefreshToken: "old"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := source.Token(context.Background(), false); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	var saved entity.Identity
	store.Load("auth", &saved)
	if calls.Load() != 1 || saved.RefreshToken != "new" {
		t.Fatal(calls.Load(), saved.RefreshToken)
	}
}

func TestRefreshReportsHTTPStatus(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	}))
	defer s.Close()
	source := &TokenSource{Store: &staterepo.Store{Dir: t.TempDir()}, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "r"}}
	if _, err := source.Token(context.Background(), true); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatal(err)
	}
}

func jwt(exp time.Time) string {
	return "x." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".x"
}

// rotatingServer accepts each refresh token once and answers reuse with
// shouldLogout in a successful response, the only place the desktop reads
// it. extra is merged into every response.
func rotatingServer(t *testing.T, extra map[string]any) (*httptest.Server, *atomic.Int32) {
	var mu sync.Mutex
	var calls atomic.Int32
	used := map[string]bool{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		old := body["refresh_token"]
		if used[old] {
			json.NewEncoder(w).Encode(map[string]bool{"shouldLogout": true})
			return
		}
		used[old] = true
		resp := map[string]any{"access_token": "access-" + old + "'", "refresh_token": old + "'"}
		for k, v := range extra {
			resp[k] = v
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

func TestRefreshKeepsRotatedTokenWhenSaveFails(t *testing.T) {
	s, _ := rotatingServer(t, nil)
	// A store whose directory is a regular file cannot be locked or saved.
	dir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(dir, nil, 0o600)
	source := &TokenSource{Store: &staterepo.Store{Dir: dir}, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "r"}}
	for range 2 {
		if _, err := source.Token(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	if got := source.Snapshot().RefreshToken; got != "r''" {
		t.Fatal(got)
	}
}

func TestRefreshAdoptsTokensRotatedByAnotherProcess(t *testing.T) {
	s, calls := rotatingServer(t, nil)
	store := &staterepo.Store{Dir: t.TempDir()}
	start := entity.Identity{AccessToken: "a", RefreshToken: "r", AuthID: "acc"}
	store.Save("auth", start)
	first := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: start}
	second := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: start}
	if _, err := first.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	tok, err := second.Token(context.Background(), true)
	if err != nil || tok != "access-r'" || calls.Load() != 1 {
		t.Fatal(tok, err, calls.Load())
	}
	var saved entity.Identity
	store.Load("auth", &saved)
	if saved.RefreshToken != "r'" || second.Snapshot().RefreshToken != "r'" {
		t.Fatal(saved, second.Snapshot())
	}
}

func TestRefreshLeavesOtherAccountOnDisk(t *testing.T) {
	s, _ := rotatingServer(t, nil)
	store := &staterepo.Store{Dir: t.TempDir()}
	other := entity.Identity{AccessToken: "b", RefreshToken: "rb", AuthID: "account-b"}
	store.Save("auth", other)
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "ra", AuthID: "account-a"}}
	if tok, err := source.Token(context.Background(), true); err != nil || tok != "access-ra'" {
		t.Fatal(tok, err)
	}
	// Reusing the stale token is rejected, but must not log account B out.
	source.Identity.RefreshToken = "ra"
	if _, err := source.Token(context.Background(), true); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatal(err)
	}
	var saved entity.Identity
	store.Load("auth", &saved)
	if saved != other {
		t.Fatal(saved)
	}
}

func TestRefreshLogoutWipesOnlyRejectedSession(t *testing.T) {
	s, _ := rotatingServer(t, nil)
	store := &staterepo.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "r"}}
	if _, err := source.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// Another process still holds "r", and so does the disk.
	store.Save("auth", entity.Identity{RefreshToken: "r"})
	stale := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "r"}}
	if _, err := stale.Token(context.Background(), true); err == nil {
		t.Fatal("expected revocation")
	}
	var saved entity.Identity
	store.Load("auth", &saved)
	if saved != (entity.Identity{}) {
		t.Fatal("rejected session kept", saved)
	}
}

func TestTokenWithoutExpiry(t *testing.T) {
	s, calls := rotatingServer(t, map[string]any{"expires_in": 3600})
	store := &staterepo.Store{Dir: t.TempDir()}
	// An opaque token of unknown lifetime is used until the server rejects it.
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{AccessToken: "opaque", RefreshToken: "r"}}
	if tok, err := source.Token(context.Background(), false); err != nil || tok != "opaque" || calls.Load() != 0 {
		t.Fatal(tok, err, calls.Load())
	}
	// After a forced refresh, expires_in keeps the new token in use.
	if _, err := source.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if tok, err := source.Token(context.Background(), false); err != nil || tok != "access-r'" {
			t.Fatal(tok, err)
		}
	}
	if calls.Load() != 1 || source.Snapshot().ExpiresAtMS <= time.Now().UnixMilli() {
		t.Fatal(calls.Load(), source.Snapshot())
	}
	// A reported expiry inside the refresh margin triggers a refresh.
	source.Identity.ExpiresAtMS = time.Now().Add(time.Minute).UnixMilli()
	if tok, err := source.Token(context.Background(), false); err != nil || tok != "access-r''" {
		t.Fatal(tok, err)
	}
}

// Like the desktop's unauthenticated recovery, a rejected token is refreshed
// only while it is the current one, and at most once per cooldown.
func TestRejected(t *testing.T) {
	s, calls := rotatingServer(t, nil)
	source := &TokenSource{Store: &staterepo.Store{Dir: t.TempDir()}, HTTP: s.Client(), Base: s.URL,
		Identity: entity.Identity{AccessToken: "opaque", RefreshToken: "r"}}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := source.Rejected(context.Background(), "opaque"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || source.Snapshot().AccessToken != "access-r'" {
		t.Fatal(calls.Load(), source.Snapshot())
	}
	// The new token is rejected too, but within the cooldown.
	if err := source.Rejected(context.Background(), "access-r'"); err != nil || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
	source.rejectedAt = time.Now().Add(-rejectCooldown)
	if err := source.Rejected(context.Background(), "access-r'"); err != nil || calls.Load() != 2 {
		t.Fatal(err, calls.Load())
	}
}

// shouldLogout in a failed refresh is a temporary failure: the login stays.
func TestRefreshFailureKeepsLogin(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"shouldLogout":true}`)
	}))
	defer s.Close()
	store := &staterepo.Store{Dir: t.TempDir()}
	login := entity.Identity{AccessToken: "a", RefreshToken: "r"}
	store.Save("auth", login)
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: login}
	if _, err := source.Token(context.Background(), true); err == nil || errors.Is(err, entity.ErrLogout) {
		t.Fatal(err)
	}
	var saved entity.Identity
	store.Load("auth", &saved)
	if saved != login || source.Snapshot() != login {
		t.Fatal(saved, source.Snapshot())
	}
}

func TestRefreshUsesClientID(t *testing.T) {
	var got atomic.Value
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		got.Store(body["client_id"])
		json.NewEncoder(w).Encode(map[string]string{"access_token": "a2"})
	}))
	defer s.Close()
	source := &TokenSource{Store: &staterepo.Store{Dir: t.TempDir()}, HTTP: s.Client(), Base: s.URL, ClientID: "custom",
		Identity: entity.Identity{RefreshToken: "r"}}
	if _, err := source.Token(context.Background(), true); err != nil || got.Load() != "custom" {
		t.Fatal(err, got.Load())
	}
}

// A refresh outlives the caller that started it, like the desktop's, whose
// fetch obeys only its own deadline: the server has already rotated the
// refresh token, so dropping its late answer would log the user out.
func TestRefreshOutlivesCaller(t *testing.T) {
	var mu sync.Mutex
	used := map[string]bool{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		old := body["refresh_token"]
		reused := used[old]
		used[old] = true
		mu.Unlock()
		if reused {
			json.NewEncoder(w).Encode(map[string]bool{"shouldLogout": true})
			return
		}
		time.Sleep(300 * time.Millisecond) // rotated, but answered late
		json.NewEncoder(w).Encode(map[string]string{"access_token": "access-" + old + "'", "refresh_token": old + "'"})
	}))
	defer s.Close()
	store := &staterepo.Store{Dir: t.TempDir()}
	start := entity.Identity{AccessToken: jwt(time.Now().Add(-time.Minute)), RefreshToken: "r", AuthID: "acc"}
	store.Save("auth", start)
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: start}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := source.Token(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// The next caller shares the refresh still in flight.
	if tok, err := source.Token(context.Background(), false); err != nil || tok != "access-r'" {
		t.Fatal(tok, err)
	}
	var saved entity.Identity
	store.Load("auth", &saved)
	if saved.RefreshToken != "r'" || source.Snapshot().RefreshToken != "r'" {
		t.Fatal(saved, source.Snapshot())
	}
}

// A refresh whose caller is gone still saves the answer.
func TestRefreshSavesAfterCallerLeft(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"access_token": "a2", "refresh_token": "r2"})
	}))
	defer s.Close()
	store := &staterepo.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "r"}}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := source.Token(ctx, true)
		errc <- err
	}()
	// The caller leaves once the refresh is under way.
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var saved entity.Identity
		store.Load("auth", &saved)
		if saved.RefreshToken == "r2" && source.Snapshot().AccessToken == "a2" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(saved, source.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// O3-1: Wait returns only once the refresh its callers left has saved its
// late answer, so that a process exiting after Wait keeps the rotated
// refresh token.
func TestWaitSavesLateAnswer(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"access_token": "a2", "refresh_token": "r2"})
	}))
	defer s.Close()
	store := &staterepo.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{RefreshToken: "r"}}
	if err := source.Wait(context.Background()); err != nil {
		t.Fatal("no refresh to wait for:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go source.Token(ctx, true)
	<-started
	cancel()
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if err := source.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Wait returned before the refresh ended:", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- source.Wait(context.Background()) }()
	select {
	case err := <-waited:
		t.Fatal("Wait returned before the refresh ended:", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	var saved entity.Identity
	store.Load("auth", &saved)
	if saved.RefreshToken != "r2" || source.Snapshot().AccessToken != "a2" {
		t.Fatal(saved, source.Snapshot())
	}
}

// O3-1: a caller that is already gone starts no refresh: nobody would wait
// for it, and its process may be exiting.
func TestCanceledCallerStartsNoRefresh(t *testing.T) {
	s, calls := rotatingServer(t, nil)
	store := &staterepo.Store{Dir: t.TempDir()}
	source := &TokenSource{Store: store, HTTP: s.Client(), Base: s.URL, Identity: entity.Identity{AccessToken: "a", RefreshToken: "r"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Token(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := source.Rejected(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := source.Wait(context.Background()); err != nil || calls.Load() != 0 {
		t.Fatal(err, calls.Load())
	}
	// The canceled Rejected armed no cooldown.
	if err := source.Rejected(context.Background(), "a"); err != nil || calls.Load() != 1 || source.Snapshot().RefreshToken != "r'" {
		t.Fatal(err, calls.Load(), source.Snapshot())
	}
}
