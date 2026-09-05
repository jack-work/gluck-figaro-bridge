package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		if b.attends == "" {
			return "no aria bound: send a message and one is minted", "", true
		}
		return "bound to `" + b.attends + "`" + b.describe(ctx, b.attends), "", true

	case "/arias":
		list, err := b.figaro.List(ctx, 12)
		if err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		var lines []string
		for _, a := range list {
			line := "`" + a.ID + "`"
			if a.Mantra != "" {
				line += "  " + a.Mantra
			}
			if a.ID == b.attends {
				line = "▸ " + line
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			return "no arias", "", true
		}
		return strings.Join(lines, "\n"), "", true

	case "/attend", "/bind":
		if rest == "" {
			return "usage: /attend <aria-id|@role>", "", true
		}
		target := strings.Fields(rest)[0]

		if IsRole(target) {
			held, err := b.figaro.ResolveRole(ctx, target)
			if err != nil {
				return "⚠️ " + err.Error(), "", true
			}
			b.attend(target)
			return "attending role `" + target + "`\nheld by `" + held + "`" +
				b.describe(ctx, held), "", true
		}
		if !b.figaro.Exists(ctx, target) {
			return "⚠️ no such aria: `" + target + "`", "", true
		}
		b.attend(target)
		return "attending `" + target + "`" + b.describe(ctx, target), "", true

	case "/new":
		id, err := b.figaro.Create(ctx)
		if err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		b.attend(id)
		return "new aria `" + id + "`", rest, true

	case "/roles":
		roles, err := b.figaro.Roles(ctx)
		if err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		if len(roles) == 0 {
			return "no roles", "", true
		}
		var lines []string
		for _, r := range roles {
			line := "`" + r.FormID + "`"
			if r.Name != "" {
				line += "  " + r.Name
			}
			line += "  →  `" + r.TargetAria + "`"
			if r.FormID == b.attends {
				line = "▸ " + line
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n"), "", true

	case "/cut":
		target, err := b.target(ctx)
		if err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		if err := b.figaro.Interrupt(ctx, target); err != nil {
			return "⚠️ " + err.Error(), "", true
		}
		return "cut.", "", true
	}
	return "unknown command " + verb + "\n\n" + helpText, "", true
}

// describe returns a short ": <mantra>" suffix, or "" if unavailable. Best
// effort: naming the aria is the point, and the mantra is a bonus.
func (b *bridge) describe(ctx context.Context, aria string) string {
	if m := b.figaro.Mantra(ctx, aria); m != "" {
		return ": " + m
	}
	return ""
}

// attend points this chat at an aria or a role and remembers it. Attending
// something new re-briefs, so the new aria is told how to answer.
func (b *bridge) attend(target string) {
	b.attends = target
	b.briefed = false
	if b.binding != nil {
		b.binding.set(target)
	}
}

// target resolves what this chat attends to a concrete aria id, following a
// role to whoever currently holds it.
func (b *bridge) target(ctx context.Context) (string, error) {
	if b.attends == "" {
		return "", fmt.Errorf("attending nothing")
	}
	if IsRole(b.attends) {
		return b.figaro.ResolveRole(ctx, b.attends)
	}
	return b.attends, nil
}
