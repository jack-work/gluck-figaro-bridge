package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// Reply watching.
//
// Messages are delivered to figaro immediately, with `send --forget`, so a
// second thought never waits for the first turn to finish. Figaro already has
// a queue and does the right thing with it: a message arriving mid-turn is
// injected as a STEERING node into the running turn rather than starting a
// new one, so "and also check X" lands while the aria is still working.
//
// That is why replies cannot be collected by the sender. A blocking `send`
// attaches to the aria's live stream, not to "the answer to my message", so
// two concurrent senders both receive whatever the aria says next — verified,
// and it loses one reply while duplicating the other.
//
// So delivery and reply are separate concerns: fire messages in, watch the
// aria for prose, forward what appears.

type ariaState struct {
	turn      int
	nodeCount int
}

// watchReplies forwards new aria output to herald until ctx is done.
func (b *bridge) watchReplies(ctx context.Context, poll time.Duration) {
	// Start from wherever the aria already is, so a restart does not repeat
	// the last reply it happened to find.
	seen, err := b.snapshot(ctx)
	if err != nil {
		log.Printf("reply watcher: initial snapshot: %v (starting from empty)", err)
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if b.aria == "" {
			continue
		}

		idle, err := b.idle(ctx)
		if err != nil || !idle {
			// Mid-turn: nothing is final yet, and forwarding a partial reply
			// would send you half a thought.
			continue
		}

		text, now, err := b.newProse(ctx, seen)
		if err != nil {
			log.Printf("reply watcher: %v", err)
			continue
		}
		if strings.TrimSpace(text) == "" {
			seen = now
			continue
		}

		sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err = b.herald.Say(sendCtx, b.to, text)
		cancel()
		if err != nil {
			// Do not advance the marker: retry this reply on the next tick
			// rather than dropping it.
			log.Printf("reply watcher: send: %v", err)
			continue
		}
		log.Printf("replied %d bytes from turn %d", len(text), now.turn)
		seen = now
	}
}

func (b *bridge) idle(ctx context.Context) (bool, error) {
	out, err := b.figaroOut(ctx, 20*time.Second, "-A", "status", b.aria, "-j")
	if err != nil {
		return false, err
	}
	var st struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return false, fmt.Errorf("status: %w", err)
	}
	return st.State == "idle", nil
}

// snapshot records where the aria currently is, without forwarding anything.
func (b *bridge) snapshot(ctx context.Context) (ariaState, error) {
	part, err := b.lastPart(ctx)
	if err != nil {
		return ariaState{}, err
	}
	return ariaState{turn: part.Turn, nodeCount: len(part.Nodes)}, nil
}

type showPart struct {
	Turn  int `json:"turn"`
	Nodes []struct {
		Type     string `json:"type"`
		Role     string `json:"role"`
		Markdown string `json:"markdown"`
	} `json:"nodes"`
}

func (b *bridge) lastPart(ctx context.Context) (showPart, error) {
	out, err := b.figaroOut(ctx, 30*time.Second, "-A", "show", b.aria, "-n", "1", "-j")
	if err != nil {
		return showPart{}, err
	}
	var doc struct {
		Parts []showPart `json:"parts"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return showPart{}, fmt.Errorf("show: %w", err)
	}
	if len(doc.Parts) == 0 {
		return showPart{}, nil
	}
	return doc.Parts[len(doc.Parts)-1], nil
}

// newProse returns the aria's prose that has appeared since seen, and the
// marker to record once it has been delivered.
//
// Only `prose` output nodes are forwarded: thinking is private, tool calls
// are noise on a phone, and steering nodes are the user's own words coming
// back.
func (b *bridge) newProse(ctx context.Context, seen ariaState) (string, ariaState, error) {
	part, err := b.lastPart(ctx)
	if err != nil {
		return "", seen, err
	}
	now := ariaState{turn: part.Turn, nodeCount: len(part.Nodes)}

	from := 0
	if part.Turn == seen.turn {
		from = seen.nodeCount
	}
	if from > len(part.Nodes) {
		from = len(part.Nodes)
	}

	var out []string
	for _, n := range part.Nodes[from:] {
		if n.Type == "prose" && n.Role == "output" {
			if t := strings.TrimSpace(n.Markdown); t != "" {
				out = append(out, t)
			}
		}
	}
	return strings.Join(out, "\n\n"), now, nil
}
