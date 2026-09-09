package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jack-work/figaro/api/rpc"
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

	// onNotify receives daemon pushes on every aria connection. Used to
	// drive the typing indicator: a connection that only sends would learn
	// nothing about when work starts.
	onNotify sdk.NotifyHandler

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

	// Attach through the angelus, and RETRY ONCE ON A FRESH CONNECTION.
	//
	// A cached socket cannot be trusted to still be there: the daemon is
	// restarted by upgrades, by `figaro stop`, and by the memory watchdog,
	// none of which the bridge is told about. Retrying once costs a redial
	// on a genuine error (an aria that really is missing) and self-heals the
	// case that otherwise never recovers.
	att, err := f.attachOnce(ctx, id)
	if err != nil {
		f.forgetAngelus()
		att, err = f.attachOnce(ctx, id)
	}
	if err != nil {
		return nil, fmt.Errorf("attach %s: %w", id, err)
	}
	c, err := sdk.DialAria(transport.UnixEndpoint(att.Endpoint.Address), f.onNotify)
	if err != nil {
		return nil, fmt.Errorf("dial aria %s: %w", id, err)
	}

	f.mu.Lock()
	f.arias[id] = c
	f.mu.Unlock()
	return c, nil
}

// attachOnce is one attempt: dial the angelus (or reuse the cached one) and
// ask it where the aria's endpoint is.
func (f *figaroClient) attachOnce(ctx context.Context, id string) (*rpc.AttachResponse, error) {
	ang, err := f.dialAngelus()
	if err != nil {
		return nil, err
	}
	return ang.Attach(ctx, id)
}

// forgetAngelus drops the cached angelus connection so the next call redials.
//
// THIS IS THE ONE THAT WAS MISSING, and its absence was a permanent outage
// rather than a slow path. The bridge held one angelus connection for its
// whole life; when the daemon restarted underneath it (2026-09-09: bridge up
// since the 8th, daemon restarted the next evening) every Attach failed with
// `connection closed`, forever, because nothing ever redialled. The aria
// cache had forget() from the start; the angelus never did.
func (f *figaroClient) forgetAngelus() {
	f.mu.Lock()
	if f.angelus != nil {
		_ = f.angelus.Close()
		f.angelus = nil
	}
	f.mu.Unlock()
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

// Hangup stops the running turn and says what happened to the queue.
//
// The disposition is the whole difference between the two things a phone
// might mean by "stop": keep what is waiting, or discard it. The daemon
// returns the queue either way, which is what lets the reply name what
// survived or hand back verbatim what was thrown away. A dropped message the
// sender cannot see again is worse than a turn that kept running.
func (f *figaroClient) Hangup(ctx context.Context, id string, disposition rpc.QueueDisposition) (*rpc.InterruptResponse, error) {
	c, err := f.aria(ctx, id)
	if err != nil {
		return nil, err
	}
	resp, err := c.Hangup(ctx, disposition)
	if err != nil {
		f.forget(id)
		return nil, err
	}
	return resp, nil
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

// ---------- roles ----------

// Role is an unbound form carrying target-aria: a name that points at
// whichever aria currently holds the seat.
type Role struct {
	FormID     string // "@96447061"
	Name       string
	TargetAria string
}

// Roles lists the roles the daemon knows about.
func (f *figaroClient) Roles(ctx context.Context) ([]Role, error) {
	ang, err := f.dialAngelus()
	if err != nil {
		return nil, err
	}
	list, err := ang.ListGlobal(ctx)
	if err != nil {
		return nil, err
	}
	var out []Role
	for _, a := range list.Figaros {
		if a.TargetAria == "" {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.Mantra
		}
		out = append(out, Role{FormID: a.ID, Name: name, TargetAria: a.TargetAria})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ResolveRole returns the aria a role currently points at.
//
// Deliberately resolved at every send rather than cached. A role is a seat,
// not an alias: when succession moves it to another aria, the next message
// must follow the seat. Caching the answer would pin the chat to whoever
// happened to hold it when you attended.
func (f *figaroClient) ResolveRole(ctx context.Context, formID string) (string, error) {
	id := strings.TrimPrefix(formID, "@")
	roles, err := f.Roles(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range roles {
		if strings.TrimPrefix(r.FormID, "@") == id {
			if r.TargetAria == "" {
				return "", fmt.Errorf("role %s points at no aria", formID)
			}
			return r.TargetAria, nil
		}
	}
	return "", fmt.Errorf("no role %s", formID)
}

// IsRole reports whether a target names a role rather than an aria.
func IsRole(target string) bool { return strings.HasPrefix(target, "@") }
