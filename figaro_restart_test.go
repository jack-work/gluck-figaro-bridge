package main

// THE STALE-CONNECTION TEST.
//
// This exists because its absence was a day-long outage. The bridge held one
// angelus connection for its whole life. On 2026-09-09 the daemon restarted
// underneath it -- bridge up since the 8th -- and every Attach failed with
// `connection closed`, forever, because nothing dropped the dead client. The
// aria cache had forget() from the beginning; the angelus never did.
//
// The daemon is restarted by upgrades, by `figaro stop`, and by the memory
// watchdog, none of which the bridge is told about. So "the socket I hold is
// still alive" is never a safe assumption, and this test is the one that
// says so.

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jack-work/jkrpc"
)

// fakeAngelus serves figaro.attach on a unix socket and can be killed and
// replaced, which is exactly what a daemon restart looks like from here.
type fakeAngelus struct {
	path  string
	ln    net.Listener
	calls chan string

	// conns are the ACCEPTED connections. stop() must close these too:
	// closing only the listener leaves established sockets serving happily,
	// so the "restart" would not actually break anything and the test would
	// pass against the very bug it exists to catch. A real daemon restart
	// kills the process and every connection with it.
	mu    sync.Mutex
	conns []net.Conn
}

func startFakeAngelus(t *testing.T, path string) *fakeAngelus {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	f := &fakeAngelus{path: path, ln: ln, calls: make(chan string, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go func(c net.Conn) {
				conn := jkrpc.NewConn(c)
				srv := jkrpc.NewServer(conn, map[string]jkrpc.HandlerFunc{
					"figaro.attach": func(ctx context.Context, p json.RawMessage) (any, error) {
						var req struct {
							FigaroID string `json:"figaro_id"`
						}
						_ = json.Unmarshal(p, &req)
						select {
						case f.calls <- req.FigaroID:
						default:
						}
						// Point at a socket that does not exist: this test is
						// about reaching the ANGELUS, and dialing the aria
						// afterwards is a separate failure with its own path.
						return map[string]any{
							"figaro_id": req.FigaroID,
							"endpoint":  map[string]string{"scheme": "unix", "address": "/nonexistent.sock"},
						}, nil
					},
				})
				_ = srv.Serve(context.Background())
			}(c)
		}
	}()
	return f
}

func (f *fakeAngelus) stop() {
	f.ln.Close()
	f.mu.Lock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
	f.mu.Unlock()
}

// waitCall reports whether the angelus was asked to attach within a beat.
func (f *fakeAngelus) waitCall(t *testing.T) bool {
	t.Helper()
	select {
	case <-f.calls:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func TestAttachSurvivesADaemonRestart(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "angelus.sock")
	first := startFakeAngelus(t, sock)
	t.Cleanup(first.stop)

	fc := newFigaroClient(sock)
	ctx := context.Background()

	// 1. First attach: reaches the angelus and caches the connection. The
	//    aria dial fails (the endpoint is deliberately bogus), which is fine
	//    -- what matters is that the ANGELUS was asked.
	_, _ = fc.aria(ctx, "aria0001")
	if !first.waitCall(t) {
		t.Fatal("the first attach never reached the angelus")
	}

	// 2. The daemon goes away and comes back on the same path. This is an
	//    upgrade, a `figaro stop`, or the memory watchdog.
	first.stop()
	// Give the client's read loop a moment to notice the close, so the test
	// exercises "cached client is dead" rather than "cached client is fine".
	time.Sleep(200 * time.Millisecond)

	second := startFakeAngelus(t, sock)
	t.Cleanup(second.stop)

	// 3. THE ASSERTION. With a cached-but-dead client and no invalidation,
	//    this attach fails forever and the new angelus is never contacted.
	_, _ = fc.aria(ctx, "aria0002")
	if !second.waitCall(t) {
		t.Fatal("after a daemon restart the bridge never reached the NEW angelus: " +
			"the cached connection was not invalidated, which is the bug that " +
			"made telegram silently stop working for a day")
	}
}

// A genuine error must still be reported rather than retried into silence.
// The retry costs one redial and then surfaces the real failure.
func TestAttachStillFailsWhenThereIsNoDaemon(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")
	fc := newFigaroClient(sock)
	if _, err := fc.aria(context.Background(), "aria0001"); err == nil {
		t.Fatal("attaching with no daemon at all returned no error")
	}
}

// forgetAngelus must be safe to call when there is nothing cached: it runs on
// every failure path, including the first one.
func TestForgetAngelusIsIdempotent(t *testing.T) {
	fc := newFigaroClient(filepath.Join(t.TempDir(), "x.sock"))
	fc.forgetAngelus()
	fc.forgetAngelus()
}
