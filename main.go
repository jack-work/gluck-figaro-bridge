// Command figaro-bridge routes Telegram messages into figaro and the replies
// back out, over herald.
//
// It runs on the WORKSTATION, not on spain, and that is the whole design.
// It long-polls herald's inbox and shells out to a local `figaro send`, so
// spain never reaches into the figaro store, needs no credential for this
// machine, and cannot push anything into an aria. The trust arrow points
// outward only, and herald stays ignorant of what figaro is.
//
// Herald is a dependency here; the dependency does not run the other way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	herald "github.com/jack-work/gluck-herald/client"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("figaro-bridge ")

	var (
		api    = flag.String("herald", envOr("HERALD_API", "https://herald.kelliher.info"), "herald base URL")
		aria   = flag.String("aria", os.Getenv("FIGARO_BRIDGE_ARIA"), "aria to route into (default: whatever /bind last chose)")
		stateP = flag.String("state", envOr("FIGARO_BRIDGE_STATE", defaultStatePath()), "where the current binding is remembered")
		to     = flag.String("to", envOr("FIGARO_BRIDGE_TO", "gluck"), "herald recipient for replies")
		wait   = flag.Duration("wait", 55*time.Second, "long-poll window (herald caps at 60s)")
		turnTO = flag.Duration("turn-timeout", 15*time.Minute, "kill a figaro turn that outlives this")
		once   = flag.Bool("once", false, "handle at most one batch, then exit")
		dryRun = flag.Bool("dry-run", false, "print what would be sent to figaro; call nothing")
		figBin = flag.String("figaro", envOr("FIGARO_BIN", "figaro"), "figaro binary")
	)
	flag.Parse()

	c := herald.New(*api, &herald.EnvTokenSource{Var: herald.DefaultTokenVar})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A binding survives restarts: without it a restart would silently start
	// a new conversation, which is the kind of loss only noticed later.
	bind := loadBinding(*stateP)
	if *aria == "" {
		*aria = bind.Aria
	}

	b := &bridge{
		herald: c, aria: *aria, to: *to, binding: bind,
		turnTimeout: *turnTO, figaro: *figBin, dryRun: *dryRun,
	}

	log.Printf("herald=%s aria=%s to=%s", *api, orAuto(*aria), *to)

	backoff := 5 * time.Second
	for ctx.Err() == nil {
		msgs, err := c.Inbox(ctx, 0, *wait)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("inbox: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			// Back off on sustained failure so a dead herald does not become
			// a hot loop, but recover quickly once it answers.
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second

		for _, m := range msgs {
			if err := b.handle(ctx, m); err != nil {
				log.Printf("handling %d: %v", m.ID, err)
				continue
			}
			if *dryRun {
				// A dry run must not consume the queue: acknowledging a
				// message it only pretended to handle would silently discard
				// it, which is the opposite of what --dry-run promises.
				continue
			}
			// Acknowledge only after the reply is out. Delivery is
			// at-least-once, so a crash mid-turn replays the message rather
			// than losing it.
			if err := c.Ack(ctx, m.ID); err != nil {
				log.Printf("ack %d: %v", m.ID, err)
			}
		}
		if *once {
			break
		}
	}
	log.Printf("stopped")
}

type bridge struct {
	briefed     bool
	binding     *binding
	herald      *herald.Client
	aria        string
	to          string
	turnTimeout time.Duration
	figaro      string
	dryRun      bool
}

// brief is prepended to the first message of a run, so an aria that also has
// a terminal open knows which door a prompt came through and how to answer.
const brief = "You are being addressed over Telegram, through herald. " +
	"Keep replies phone-sized: short paragraphs, no long code dumps, no ANSI. " +
	"Later messages from this chat are marked [telegram]."

func (b *bridge) handle(ctx context.Context, m herald.Message) error {
	log.Printf("from=%s: %.80q", m.From, m.Text)

	text := strings.TrimSpace(m.Text)

	// Commands are handled here rather than passed to the aria: switching
	// which aria a chat talks to cannot be the current aria's decision.
	if reply, follow, handled := b.command(ctx, text); handled {
		if b.dryRun {
			fmt.Printf("command %q -> %s\n", text, reply)
			return nil
		}
		sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		if err := b.herald.Say(sendCtx, b.to, reply); err != nil {
			cancel()
			return err
		}
		cancel()
		if strings.TrimSpace(follow) == "" {
			return nil
		}
		text = follow
	}

	prompt := "[telegram] " + text
	if !b.briefed {
		prompt = brief + "\n\n---\n\n" + prompt
		b.briefed = true
	}

	if b.dryRun {
		fmt.Printf("would send to aria %s:\n%s\n", orAuto(b.aria), prompt)
		return nil
	}

	reply, err := b.ask(ctx, prompt)
	if err != nil {
		reply = "⚠️ " + err.Error()
	}
	if strings.TrimSpace(reply) == "" {
		reply = "(no output)"
	}

	sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return b.herald.Say(sendCtx, b.to, reply)
}

// ask runs one figaro turn and returns its output.
func (b *bridge) ask(ctx context.Context, prompt string) (string, error) {
	args := []string{"-A", "send", "-r"}
	if b.aria != "" {
		args = append(args, "--id", b.aria)
	}
	args = append(args, "--", prompt)

	ctx, cancel := context.WithTimeout(ctx, b.turnTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, b.figaro, args...).Output()
	text := strings.TrimSpace(string(out))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return text, fmt.Errorf("figaro: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return text, fmt.Errorf("figaro: %w", err)
	}
	return text, nil
}

// defaultStatePath keeps the binding beside other user state.
func defaultStatePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "figaro-bridge", "binding.json")
}

func orAuto(s string) string {
	if s == "" {
		return "(pid-bound)"
	}
	return s
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
