package sandapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// X-Ghost-Mode header values.
const (
	ghostOn  = "true"
	ghostOff = "false"
)

// As in the desktop's Jxe: lookups time out after privacyTimeout (n3t), a
// result is kept for privacyCacheTTL (e3t) and a failure for
// privacyRetryTTL (t3t).
const (
	privacyTimeout  = 3 * time.Second
	privacyCacheTTL = 5 * time.Minute
	// privacyRetryTTL delays the next lookup after a failure.
	privacyRetryTTL = 10 * time.Second
)

// PrivacyHTTP returns the HTTP client for privacy lookups, based on t. It
// never consults a Privacy, so lookups are always private and cannot recurse
// into Ghost, and it does not follow redirects, so a redirected lookup fails
// (falling back to ghost mode) instead of looping.
func PrivacyHTTP(t Transport) *http.Client {
	t.Privacy = nil
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Privacy caches the account's privacy mode. Lookup failures fall back to the
// safe value, ghost mode on. Client should use PrivacyHTTP.
type Privacy struct {
	mu      sync.Mutex // guards the fields below; not held during lookups
	Client  rpc.DashboardServiceClient
	expires time.Time
	ghost   string
	pending chan struct{} // closed when the lookup in flight ends
}

// Ghost returns the X-Ghost-Mode header value: "false" only if the account
// allows its data to be used for training. Like the desktop, callers that
// come while a lookup is in flight wait for its result; one whose ctx ends
// first gets ghost mode on.
func (pv *Privacy) Ghost(ctx context.Context) string {
	pv.mu.Lock()
	if pv.Client == nil {
		pv.mu.Unlock()
		return ghostOn
	}
	if pv.ghost != "" && time.Now().Before(pv.expires) {
		ghost := pv.ghost
		pv.mu.Unlock()
		return ghost
	}
	pending := pv.pending
	if pending == nil {
		pending = make(chan struct{})
		pv.pending = pending
		// Detached from ctx: the result serves every waiting caller.
		go pv.refresh(context.WithoutCancel(ctx), pv.Client, pending)
	}
	pv.mu.Unlock()
	select {
	case <-pending:
	case <-ctx.Done():
		return ghostOn
	}
	pv.mu.Lock()
	defer pv.mu.Unlock()
	return pv.ghost
}

// refresh looks the mode up, caches it and wakes the callers waiting on
// pending.
func (pv *Privacy) refresh(ctx context.Context, client rpc.DashboardServiceClient, pending chan struct{}) {
	ghost := pv.lookup(ctx, client)
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if ghost == "" {
		pv.ghost, pv.expires = ghostOn, time.Now().Add(privacyRetryTTL)
	} else {
		pv.ghost, pv.expires = ghost, time.Now().Add(privacyCacheTTL)
	}
	pv.pending = nil
	close(pending)
}

// lookup asks the server for the privacy mode. It returns "" on failure.
func (pv *Privacy) lookup(ctx context.Context, client rpc.DashboardServiceClient) string {
	ctx, cancel := context.WithTimeout(ctx, privacyTimeout)
	defer cancel()
	req := &aiserverv1.GetUserPrivacyModeRequest{InferredPrivacyMode: aiserverv1.PrivacyMode_PRIVACY_MODE_NO_STORAGE}
	r, err := client.GetUserPrivacyMode(ctx, connect.NewRequest(req))
	if err != nil {
		return ""
	}
	switch r.Msg.PrivacyMode {
	case aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_DATA_TRAINING_ALLOWED,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_CODEBASE_TRAINING_ALLOWED:
		return ghostOff
	}
	return ghostOn
}
