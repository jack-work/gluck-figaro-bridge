// Command figaro-bridge routes Telegram messages into figaro and the replies
// back out, over herald.
//
// It runs on the WORKSTATION, not on spain, and that is the whole design.
// It long-polls herald's inbox and speaks to the local figaro daemon over its
// own socket, so spain never reaches into the figaro store, needs no
// credential for this machine, and cannot push anything into an aria. The
// trust arrow points outward only, and herald stays ignorant of what figaro
// is.
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
		api        = flag.String("herald", envOr("HERALD_API", "https://herald.kelliher.info"), "herald base URL")
		aria       = flag.String("aria", os.Getenv("FIGARO_BRIDGE_ARIA"), "aria to route into (default: whatever /bind last chose)")
		stateP     = flag.String("state", envOr("FIGARO_BRIDGE_STATE", defaultStatePath()), "where the current binding is remembered")
		to         = flag.String("to", envOr("FIGARO_BRIDGE_TO", "gluck"), "herald recipient for replies")
		wait       = flag.Duration("wait", 55*time.Second, "long-poll window (herald caps at 60s)")
		once       = flag.Bool("once", false, "handle at most one batch, then exit")
		dryRun     = flag.Bool("dry-run", false, "print what would be sent to figaro; call nothing")
		credential = flag.String("credential", envOr("FIGARO_BRIDGE_CREDENTIAL", "herald"), "hush oauth credential name")
		socket     = flag.String("angelus", envOr("FIGARO_ANGELUS_SOCKET", ""), "figaro angelus socket (default: $XDG_RUNTIME_DIR/figaro/angelus.sock)")
	)
	flag.Parse()

	// Ask the agent for the token rather than reading it from the
	// environment: this process outlives any token handed to it at exec.
	tokens := newHushTokenSource(*credential)
	c := herald.New(*api, tokens)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A binding survives restarts: without it a restart would silently start
	// a new conversation, which is the kind of loss only noticed later.
	//
	// A saved binding beats the flag. The flag is where this chat starts on a
	// machine that has never run the bridge; once /bind has been used, that
	// choice is the user's and a restart must not quietly undo it.
	bind := loadBinding(*stateP)
	if bind.Aria != "" {
		*aria = bind.Aria
	}

	// Typing is driven by daemon pushes, so the aria connection must
	// subscribe rather than only send.
	act := newActivity()
	fig := newFigaroClient(*socket)
	fig.onNotify = act.notify

	b := &bridge{
		herald: c, attends: *aria, to: *to, binding: bind,
		figaro: fig, dryRun: *dryRun,
	}

	if !*dryRun {
		go holdTyping(ctx, act, func(c2 context.Context) error {
			return c.Typing(c2, *to)
		})
	}

	log.Printf("herald=%s aria=%s to=%s", *api, orAuto(*aria), *to)

	backoff := 5 * time.Second
	for ctx.Err() == nil {
		msgs, err := c.Inbox(ctx, 0, *wait)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if isUnauthorized(err) {
				// Inbox deliberately does not auto-retry: a re-issued poll
				// could double-deliver: so refresh here and let the next
				// tick use the new token.
				if _, rerr := tokens.Refresh(ctx); rerr != nil {
					log.Printf("token refresh failed: %v", rerr)
				} else {
					log.Printf("token refreshed after 401")
					// Fall through to the backoff sleep rather than retrying
					// instantly: if the fresh token is ALSO rejected, an
					// immediate retry is a hot loop against both hush and
					// herald.
				}
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
	briefed bool
	binding *binding
	herald  *herald.Client
	attends string
	to      string
	figaro  *figaroClient
	dryRun  bool
}

// brief is prepended to the first message of a run, so an aria that also has
// a terminal open knows which door a prompt came through and how to answer.
// brief tells an aria how to answer. Nothing it prints reaches the phone on
// its own: it must say so deliberately, with `herald say`.
//
// That is the point of the design rather than a limitation of it. Tailing an
// aria's output sends half-formed thinking, tool chatter and stray newlines
// to a phone, and makes the aria a subject of observation rather than a
// correspondent. Requiring an explicit call means every message that arrives
// was meant to.
const brief = "You are being addressed over Telegram, through herald.\n\n" +
	"To reply, run: herald say --to %s --from \"<your mantra>\" <markdown>\n" +
	"(the --from label and your aria id are appended as a footer, so Gluck can\n" +
	"see which of us answered)\n" +
	"(it also reads stdin, and renders markdown as Telegram HTML)\n\n" +
	"NOTHING you print reaches the phone on its own: only what you send with " +
	"that command. Keep it phone-sized: short paragraphs, no long code dumps, " +
	"no ANSI. Later messages from this chat are marked [telegram]."

func (b *bridge) handle(ctx context.Context, m herald.Message) error {
	log.Printf("from=%s: %.80q (%d media)", m.From, m.Text, len(m.Media))

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

	// Fetch attachments BEFORE the prompt goes out, so the aria is never
	// told about a file that is not yet on this disk.
	if notes := b.fetchMedia(ctx, m); notes != "" {
		if text == "" {
			text = notes
		} else {
			text += "\n\n" + notes
		}
	}

	prompt := "[telegram] " + text
	if !b.briefed {
		prompt = fmt.Sprintf(brief, b.to) + "\n\n---\n\n" + prompt
		b.briefed = true
	}

	if b.dryRun {
		fmt.Printf("would send to aria %s:\n%s\n", orAuto(b.attends), prompt)
		return nil
	}

	// Deliver and return. Figaro owns the queue: a message arriving mid-turn
	// is injected as a steering node into the running turn, so a follow-up
	// thought reaches the aria while it is still working rather than waiting
	// behind it. Replies come back through the watcher, not from here.
	return b.deliver(ctx, prompt)
}

// deliver hands a prompt to figaro without waiting for the turn.
func (b *bridge) deliver(ctx context.Context, prompt string) error {
	if b.attends == "" {
		id, err := b.figaro.Create(ctx)
		if err != nil {
			return err
		}
		b.attend(id)
		log.Printf("attending nothing; minted %s", id)
	}
	// Resolved per message, so a role that changes hands between messages
	// delivers to whoever holds it now.
	target, err := b.target(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return b.figaro.Send(ctx, target, prompt)
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

// isUnauthorized reports a 401 from herald.
func isUnauthorized(err error) bool {
	var ae *herald.APIError
	return errors.As(err, &ae) && ae.Status == 401
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
