package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/jack-work/figaro/api/transport"
	"github.com/jack-work/figaro/sdk"
)

// figaroClient talks to the angelus daemon directly, over the same JSON-RPC
// socket the figaro CLI uses.
//
// The bridge used to shell out to `figaro send`, `figaro status -j` and
// `figaro show -j`, spawning a process per message and parsing its stdout.
// The CLI is a thin wrapper over this contract, so going through it bought
// nothing and cost a fork, a JSON round trip, and a dependency on output
// formats that are free to change. sdk lives outside internal/ precisely so
// that other programs can import it.
type figaroClient struct {
	socket string

	mu      sync.Mutex
	angelus *sdk.Angelus
	// arias caches one connection per aria. Opening a socket per message
	// would reintroduce most of what dropping the CLI was meant to avoid.
	arias map[string]*sdk.Aria
}

func newFigaroClient(socket string) *figaroClient {
	if socket == "" {
		socket = defaultAngelusSocket()
	}
	return &figaroClient{socket: socket, arias: map[string]*sdk.Aria{}}
}

func defaultAngelusSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "figaro", "angelus.sock")
}

func (f *figaroClient) dialAngelus() (*sdk.Angelus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.angelus != nil {
		return f.angelus, nil
	}
	c, err := sdk.DialAngelus(transport.UnixEndpoint(f.socket))
	if err != nil {
		return nil, fmt.Errorf("dial angelus at %s: %w", f.socket, err)
	}
	f.angelus = c
	return c, nil
}

// aria returns a client for one aria, attaching through the angelus to learn
// its agent endpoint.
func (f *figaroClient) aria(ctx context.Context, id string) (*sdk.Aria, error) {
	f.mu.Lock()
	if c, ok := f.arias[id]; ok {
		f.mu.Unlock()
		return c, nil
	}
	f.mu.Unlock()

	ang, err := f.dialAngelus()
	if err != nil {
		return nil, err
	}
	att, err := ang.Attach(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("attach %s: %w", id, err)
	}
	c, err := sdk.DialAria(transport.UnixEndpoint(att.Endpoint.Address), nil)
	if err != nil {
		return nil, fmt.Errorf("dial aria %s: %w", id, err)
	}

	f.mu.Lock()
	f.arias[id] = c
	f.mu.Unlock()
	return c, nil
}

// forget drops a cached aria connection, so the next call redials. Used when
// a call fails, since the daemon may have restarted underneath us.
func (f *figaroClient) forget(id string) {
	f.mu.Lock()
	if c, ok := f.arias[id]; ok {
		_ = c.Close()
		delete(f.arias, id)
	}
	f.mu.Unlock()
}

// Send delivers a prompt without waiting for the turn.
//
// The daemon classifies it: a prompt accepted while a turn is active becomes
// steering and joins that turn, one accepted while idle opens a new one. That
// is why a follow-up thought reaches the aria while it is still working
// instead of queueing behind it.
func (f *figaroClient) Send(ctx context.Context, id, text string) error {
	c, err := f.aria(ctx, id)
	if err != nil {
		return err
	}
	if _, _, err := c.Qua(ctx, text, nil); err != nil {
		f.forget(id)
		return fmt.Errorf("send to %s: %w", id, err)
	}
	return nil
}

// Exists reports whether an aria id is real, for /bind.
func (f *figaroClient) Exists(ctx context.Context, id string) bool {
	ang, err := f.dialAngelus()
	if err != nil {
		return false
	}
	_, err = ang.Attach(ctx, id)
	return err == nil
}

// Create mints a new aria and returns its id.
func (f *figaroClient) Create(ctx context.Context) (string, error) {
	ang, err := f.dialAngelus()
	if err != nil {
		return "", err
	}
	resp, err := ang.Create(ctx, nil, nil)
	if err != nil {
		return "", fmt.Errorf("create aria: %w", err)
	}
	return resp.FigaroID, nil
}

// Interrupt stops the running turn.
func (f *figaroClient) Interrupt(ctx context.Context, id string) error {
	c, err := f.aria(ctx, id)
	if err != nil {
		return err
	}
	if err := c.Interrupt(ctx); err != nil {
		f.forget(id)
		return err
	}
	return nil
}

// Mantra returns an aria's mantra, or "" when it has none.
func (f *figaroClient) Mantra(ctx context.Context, id string) string {
	ang, err := f.dialAngelus()
	if err != nil {
		return ""
	}
	list, err := ang.List(ctx)
	if err != nil {
		return ""
	}
	for _, a := range list.Figaros {
		if a.ID == id {
			return a.Mantra
		}
	}
	return ""
}

// ariaSummary is one row of /arias.
type ariaSummary struct {
	ID         string
	Mantra     string
	LastActive int64
}

// List returns recent arias, most recently active first.
func (f *figaroClient) List(ctx context.Context, limit int) ([]ariaSummary, error) {
	ang, err := f.dialAngelus()
	if err != nil {
		return nil, err
	}
	list, err := ang.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ariaSummary, 0, len(list.Figaros))
	for _, a := range list.Figaros {
		out = append(out, ariaSummary{ID: a.ID, Mantra: a.Mantra, LastActive: a.LastActive})
	}
	// Most recently active first: that is the order a person scanning for
	// "the one I was just in" actually wants.
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastActive > out[j].LastActive })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
