package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// hushTokenSource fetches the bearer from the hush agent, and re-fetches when
// the server rejects one.
//
// A long-running process cannot take its credential from the environment the
// way a short CLI invocation does: the token it was handed at exec time
// expires within the hour, and nothing refills the variable. The agent owns
// the refresh loop, so the honest thing is to ask it again rather than to
// cache a value with a shelf life.
//
// Cheap enough to do on demand: the agent already holds a live token, so this
// is a unix-socket round trip and no network at all.
type hushTokenSource struct {
	// Get returns the cached access token.
	Get []string
	// Renew forces a refresh and returns a new access token.
	//
	// These are separate because hush's contract makes them different
	// operations: OAuthGet never blocks on a refresh, so an expired token is
	// returned as-is and the caller is expected to notice the 401 and force
	// the refresh itself. Asking `get` twice would simply hand back the same
	// dead token.
	Renew []string
	// TTL bounds how long a fetched token is reused before asking again. It
	// is a courtesy to the agent, not a correctness property: a rejected
	// token is refetched regardless of how fresh this thinks it is.
	TTL time.Duration

	mu      sync.Mutex
	token   string
	fetched time.Time
}

func newHushTokenSource(credential string) *hushTokenSource {
	return &hushTokenSource{
		Get:   []string{"hush", "oauth", "get", credential},
		Renew: []string{"hush", "oauth", "refresh", credential},
		TTL:   5 * time.Minute,
	}
}

func (h *hushTokenSource) Token(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.token != "" && time.Since(h.fetched) < h.TTL {
		return h.token, nil
	}
	return h.runLocked(ctx, h.Get)
}

// Refresh forces the agent to mint a new token. Called after a 401: a token
// can be valid by the clock and still rejected — revoked, or signed by a
// rotated key — and only the 401 knows.
func (h *hushTokenSource) Refresh(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = ""
	return h.runLocked(ctx, h.Renew)
}

func (h *hushTokenSource) runLocked(ctx context.Context, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", errors.New("hush returned no token — is this credential registered? (hush oauth device-login)")
	}
	h.token = tok
	h.fetched = time.Now()
	return tok, nil
}
