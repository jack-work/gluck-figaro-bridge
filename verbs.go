package main

// THE COMMAND TABLE.
//
// One table, not a switch plus a help string. The two drifted immediately
// when they were separate: the help advertised `/bind <id>` -- a verb figaro
// itself uses for something else entirely -- and never mentioned /attend,
// /roles or /stop, all of which the switch handled. Help is now GENERATED
// from the table, so a verb that exists is documented and a verb that is
// documented exists.
//
// THE NAMING RULE, and it is the whole point of this file: A BRIDGE VERB IS
// THE CLI'S VERB. No second dialect. `/attend` is `figaro attend`, `/state`
// is `figaro state`, `/ls` is `figaro ls`. Where the CLI has an alias the
// bridge has the same alias and no others. That is what makes the phone and
// the terminal one interface rather than two things to remember.
//
// LEGACY VERBS ARE GONE, not deprecated:
//
//   /bind  removed. It meant "attend" here while figaro's own `bind` births a
//          figaro FROM AN UNBOUND FORM. A shortcut that contradicts the tool
//          it fronts is worse than no shortcut, and aliasing it forever would
//          teach the wrong verb to the one person using it.
//   /stop  removed. `figaro stop` shuts down the DAEMON; this stopped a turn.
//          Same collision, same answer.
//
// Both now answer with what they collided with, once, so the muscle memory is
// corrected rather than silently obeyed.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jack-work/figaro/api/rpc"
)

// cmd is one verb.
type cmd struct {
	// name includes the slash. aliases are additional spellings, and exist
	// ONLY where the CLI has the same alias.
	name    string
	aliases []string
	// args is the usage suffix, empty for a bare verb.
	args string
	// help is one line, phone-width.
	help string
	// cli names the equivalent terminal command, shown in /help so the two
	// surfaces teach each other.
	cli string
	// run returns the reply, an optional prompt to send afterwards, or both.
	run func(b *bridge, ctx context.Context, rest string) (reply, prompt string)
}

// retired is a verb that USED to work and now explains itself. It is not an
// alias: it refuses, names the collision, and points at the replacement.
type retired struct {
	name, insteadOf, why string
}

var retiredVerbs = []retired{
	{"/bind", "/attend",
		"figaro's own `bind` births a figaro from an unbound form, which is a different thing"},
	{"/stop", "/hup",
		"figaro's own `stop` shuts down the daemon, not a turn"},
}

// commands is the surface. Order is the order /help prints.
//
// A FUNCTION, not a var. /help renders from this table, so the table refers to
// helpText and helpText refers back to the table. Go rejects that as an
// initialization cycle for a VAR but accepts it between functions, which is
// the honest shape anyway: the table is a description, not state.
func commands() []cmd {
	return []cmd{
		{
			name: "/help", aliases: []string{"/start"},
			help: "this list", cli: "figaro help",
			run: runHelp,
		},
		{
			name: "/aria",
			help: "which aria this chat is bound to", cli: "figaro status",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				if b.attends == "" {
					return "no aria bound: send a message and one is minted", ""
				}
				return "bound to `" + b.attends + "`" + b.describe(ctx, b.attends), ""
			},
		},
		{
			name: "/ls", aliases: []string{"/arias"},
			help: "recent arias to choose from", cli: "figaro ls",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				return b.listArias(ctx), ""
			},
		},
		{
			name: "/attend", args: "`<id|@role>`",
			help: "point this chat at an aria or a role", cli: "figaro attend",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				return b.attendCmd(ctx, rest), ""
			},
		},
		{
			name: "/new", args: "[text]",
			help: "mint a fresh aria, bind to it, and optionally prompt it", cli: "figaro new",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				id, err := b.figaro.Create(ctx)
				if err != nil {
					return "⚠️ " + err.Error(), ""
				}
				b.attend(id)
				return "new aria `" + id + "`", rest
			},
		},
		{
			name: "/roles",
			help: "roles, and who currently holds each", cli: "figaro form ls",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				return b.listRoles(ctx), ""
			},
		},
		{
			name: "/hup",
			help: "stop the running turn, keep anything queued", cli: "figaro hup",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				return b.hangup(ctx, rpc.QueueKeep), ""
			},
		},
		{
			name: "/cut",
			help: "stop it and discard the queue, handed back to you", cli: "figaro cut",
			run: func(b *bridge, ctx context.Context, rest string) (string, string) {
				return b.hangup(ctx, rpc.QueueClear), ""
			},
		},
	}
}

// runHelp is a named function rather than a closure in the table: a literal
// there would make `commands` refer to helpText and helpText refer back to
// `commands`, which Go rejects as an initialization cycle.
func runHelp(b *bridge, ctx context.Context, rest string) (string, string) {
	return helpText(), ""
}

// lookup finds a verb by any of its spellings.
func lookup(verb string) (cmd, bool) {
	for _, c := range commands() {
		if c.name == verb {
			return c, true
		}
		for _, a := range c.aliases {
			if a == verb {
				return c, true
			}
		}
	}
	return cmd{}, false
}

// helpText is GENERATED, so it cannot drift from what the table serves.
func helpText() string {
	var b strings.Builder
	b.WriteString("**figaro-bridge**\n\nJust type to talk to the bound aria.\n\n")
	for _, c := range commands() {
		b.WriteString(c.name)
		if len(c.aliases) > 0 {
			b.WriteString(" (" + strings.Join(c.aliases, ", ") + ")")
		}
		if c.args != "" {
			b.WriteString(" " + c.args)
		}
		b.WriteString(": " + c.help + "\n")
	}
	b.WriteString("\nEvery verb is figaro's own verb, so what works here works " +
		"in a terminal.\n\nReplies are not automatic: an aria answers you by " +
		"running `herald say`, so it speaks when it means to.")
	return b.String()
}

// telegramCommands is the menu Telegram shows in its command list. Derived
// from the same table, so the menu, the help and the dispatch cannot disagree.
func telegramCommands() [][2]string {
	out := make([][2]string, 0, len(commands()))
	for _, c := range commands() {
		out = append(out, [2]string{strings.TrimPrefix(c.name, "/"), c.help})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
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

	// A retired verb is answered by name. Silently aliasing it would keep
	// teaching a spelling that means something else in the tool this fronts.
	for _, r := range retiredVerbs {
		if r.name == verb {
			return fmt.Sprintf("`%s` is gone — use `%s`.\n\n%s.",
				r.name, r.insteadOf, r.why), "", true
		}
	}

	c, ok := lookup(verb)
	if !ok {
		return "unknown command " + verb + "\n\n" + helpText(), "", true
	}
	reply, prompt = c.run(b, ctx, rest)
	return reply, prompt, true
}
