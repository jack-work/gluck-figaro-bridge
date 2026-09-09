package main

// THE DRIFT TESTS.
//
// These exist because the help and the dispatch WERE two places, and they
// disagreed: /help advertised `/bind <id>` while the switch also served
// /attend, /roles and /stop, none of which were documented. Nobody noticed
// because nothing could notice. Now help is generated from the table, and
// these assert the properties that generation is supposed to guarantee.

import (
	"context"
	"strings"
	"testing"
)

// Every verb the table serves must appear in /help, and every verb /help
// mentions must be servable. This is the drift that shipped.
func TestHelpAndTableAgree(t *testing.T) {
	help := helpText()
	for _, c := range commands() {
		if !strings.Contains(help, c.name) {
			t.Errorf("%s is served but not in /help", c.name)
		}
		for _, a := range c.aliases {
			if !strings.Contains(help, a) {
				t.Errorf("%s (alias of %s) is served but not in /help", a, c.name)
			}
		}
	}
	// And nothing in the help text looks like a verb the table cannot serve.
	for _, line := range strings.Split(help, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		verb := strings.Fields(line)[0]
		verb = strings.TrimSuffix(verb, ":")
		if _, ok := lookup(verb); !ok {
			t.Errorf("/help advertises %q but nothing serves it", verb)
		}
	}
}

// THE LEGACY VERBS MUST BE GONE, not quietly aliased. Each one collided with
// a figaro verb that means something else, so obeying it would keep teaching
// the wrong spelling.
func TestRetiredVerbsRefuseAndExplain(t *testing.T) {
	b := &bridge{}
	for _, r := range retiredVerbs {
		// It must not be servable.
		if _, ok := lookup(r.name); ok {
			t.Errorf("%s is still in the command table; it was supposed to be removed", r.name)
		}
		// And it must answer with the replacement rather than "unknown".
		reply, _, handled := b.command(context.Background(), r.name)
		if !handled {
			t.Errorf("%s was not handled at all", r.name)
			continue
		}
		if !strings.Contains(reply, r.insteadOf) {
			t.Errorf("%s does not point at %s: %q", r.name, r.insteadOf, reply)
		}
		if strings.Contains(reply, "unknown command") {
			t.Errorf("%s answers as unknown; it should explain the collision", r.name)
		}
	}
}

// The naming rule, asserted: a bridge verb is the CLI's verb. Every entry
// declares its terminal equivalent, so the two surfaces stay one interface.
func TestEveryVerbNamesItsCLIEquivalent(t *testing.T) {
	for _, c := range commands() {
		if c.cli == "" {
			t.Errorf("%s names no CLI equivalent: if it has none, say why in a comment "+
				"rather than leaving the field blank", c.name)
		}
		if !strings.HasPrefix(c.cli, "figaro ") {
			t.Errorf("%s claims CLI equivalent %q, which is not a figaro command", c.name, c.cli)
		}
	}
}

// Telegram's menu is derived from the same table, so it cannot advertise a
// verb the bridge does not serve.
func TestTelegramMenuMatchesTheTable(t *testing.T) {
	menu := telegramCommands()
	if len(menu) != len(commands()) {
		t.Fatalf("menu has %d entries, table has %d", len(menu), len(commands()))
	}
	for _, m := range menu {
		if _, ok := lookup("/" + m[0]); !ok {
			t.Errorf("menu advertises /%s but nothing serves it", m[0])
		}
		if m[1] == "" {
			t.Errorf("/%s has no description in the menu", m[0])
		}
	}
}

// Aliases must not collide, or lookup order silently decides which verb wins.
func TestNoDuplicateSpellings(t *testing.T) {
	seen := map[string]string{}
	for _, c := range commands() {
		for _, spelling := range append([]string{c.name}, c.aliases...) {
			if owner, dup := seen[spelling]; dup {
				t.Errorf("%s is claimed by both %s and %s", spelling, owner, c.name)
			}
			seen[spelling] = c.name
		}
	}
	for _, r := range retiredVerbs {
		if owner, live := seen[r.name]; live {
			t.Errorf("%s is retired but also served by %s", r.name, owner)
		}
	}
}

// An unknown verb must still be handled -- silence on a phone reads as a
// message that never arrived.
func TestUnknownVerbIsAnswered(t *testing.T) {
	b := &bridge{}
	reply, _, handled := b.command(context.Background(), "/nonsense")
	if !handled || reply == "" {
		t.Fatal("an unknown command produced no reply")
	}
	if !strings.Contains(reply, "/help") && !strings.Contains(reply, "figaro-bridge") {
		t.Errorf("an unknown command does not offer help: %q", reply)
	}
}

// Plain text is not a command and must fall through to the aria.
func TestPlainTextIsNotACommand(t *testing.T) {
	b := &bridge{}
	if _, _, handled := b.command(context.Background(), "hello there"); handled {
		t.Fatal("plain text was treated as a command")
	}
}
