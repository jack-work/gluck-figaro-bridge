package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Slash commands.
//
// The bridge is a thin shell around `figaro`, so these are deliberately few:
// enough to see what exists, point this chat at a different aria, and stop a
// runaway turn. Anything else is a message.

const helpText = `**figaro-bridge**

Just type to talk to the bound aria.

/aria: which aria this chat is bound to
/arias: recent arias to choose from
/bind ` + "`<id>`" + `, point this chat at an existing aria
/new: mint a fresh aria and bind to it
/cut: stop the running turn
/help: this list

Replies are not automatic: an aria answers you by running
` + "`herald say`" + `, so it speaks when it means to.`

// binding remembers which aria this chat talks to, across restarts. Without
// it a restart would silently start a new conversation, which is the kind of
// data loss that is only noticed later.
type binding struct {
	path string
	Aria string `json:"aria"`
}

func loadBinding(path string) *binding {
	b := &binding{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, b)
	}
	return b
}

func (b *binding) set(aria string) {
	b.Aria = aria
	if b.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(b)
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, b.path)
	}
}

// command handles a slash command, returning the reply to send and whether
// the message was a command at all. A command that also carries text (`/new
// hello`) returns that text as a prompt to run afterwards.
func (b *bridge) command(ctx context.Context, text string) (reply string, prompt string, handled bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	verb, rest, _ := strings.Cut(text, " ")
	verb = strings.ToLower(verb)
	rest = strings.TrimSpace(rest)

	switch verb {
	case "/help", "/start":
		return helpText, "", true

	case "/aria":
		if b.aria == "" {
			return "no aria bound: send a message and one is minted", "", true
		}
		return "bound to `" + b.aria + "`" + b.describe(ctx, b.aria), "", true

	case "/arias":
		out, err := b.figaroOut(ctx, 30*time.Second, "-A", "list", "-n", "12")
		if err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		return "```\n" + strings.TrimSpace(out) + "\n```", "", true

	case "/bind":
		if rest == "" {
			return "usage: /bind <aria-id>", "", true
		}
		id := strings.Fields(rest)[0]
		if _, err := b.figaroOut(ctx, 30*time.Second, "-A", "status", id, "-j"); err != nil {
			return "⚠️ no such aria: `" + id + "`", "", true
		}
		b.bind(id)
		return "bound to `" + id + "`" + b.describe(ctx, id), "", true

	case "/new":
		out, err := b.figaroOut(ctx, 60*time.Second, "-A", "new", "-j")
		if err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		var res struct {
			AriaID string `json:"aria_id"`
		}
		line := strings.TrimSpace(out)
		if i := strings.LastIndex(line, "\n"); i >= 0 {
			line = strings.TrimSpace(line[i+1:])
		}
		if err := json.Unmarshal([]byte(line), &res); err != nil || res.AriaID == "" {
			return "⚠️ could not read the new aria id from: " + out, "", true
		}
		b.bind(res.AriaID)
		return "new aria `" + res.AriaID + "`", rest, true

	case "/cut":
		if b.aria == "" {
			return "no aria bound", "", true
		}
		if _, err := b.figaroOut(ctx, 30*time.Second, "-A", "cut", b.aria); err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		return "cut.", "", true
	}
	return "unknown command " + verb + "\n\n" + helpText, "", true
}

// describe returns a short ", <mantra>" suffix, or "" if unavailable. Best
// effort: naming the aria is the point, and the mantra is a bonus.
func (b *bridge) describe(ctx context.Context, aria string) string {
	out, err := b.figaroOut(ctx, 15*time.Second, "-A", "status", aria, "-j")
	if err != nil {
		return ""
	}
	var meta struct {
		Mantra string `json:"mantra"`
	}
	if json.Unmarshal([]byte(out), &meta) != nil || meta.Mantra == "" {
		return ""
	}
	return ", " + meta.Mantra
}

// bind points this chat at an aria and remembers it. Rebinding re-briefs, so
// the new aria is told how to answer.
func (b *bridge) bind(aria string) {
	b.aria = aria
	b.briefed = false
	if b.binding != nil {
		b.binding.set(aria)
	}
}

func (b *bridge) figaroOut(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, b.figaro, args...).Output()
	if err != nil {
		return string(out), fmt.Errorf("figaro %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
