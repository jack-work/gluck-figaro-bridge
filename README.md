# figaro-bridge

Routes Telegram messages into a figaro aria and the replies back out, over
herald.

## Why it lives on the workstation

```
Telegram ──▶ herald (spain) ──┐
                              │ GET /v1/inbox   (the laptop PULLS)
                              ▼
                        figaro-bridge ──▶ figaro send   (local)
                              │
                              └── POST /v1/say ──▶ herald ──▶ Telegram
```

The laptop pulls. spain never reaches into the figaro store, needs no
credential for this machine, and cannot push anything into an aria: the trust
arrow points outward only. It is also why herald could ship before figaro ever
runs on spain.

herald is a dependency of this program; the dependency does not run the other
way. Herald knows nothing about figaro, and that is deliberate.

## Roles

Needs both herald roles, `say` and `inbox`, because it is the one caller
that genuinely reads your replies. Compare `kcal-notify`, which holds `say`
alone and therefore cannot.

## Commands

| | |
|---|---|
| `/help` | the list |
| `/aria` | which aria this chat is bound to |
| `/arias` | recent arias to choose from |
| `/bind <id>` | point this chat at an existing aria |
| `/new` | mint a fresh aria and bind to it |
| `/cut` | stop the running turn |

Commands are handled by the bridge, not passed to the aria: which aria a chat
talks to cannot be the current aria's decision.

The binding is remembered across restarts. Without that, a restart would
silently start a new conversation: the kind of loss only noticed later.

## Run

```sh
hush figaro-bridge --aria 8fc9ebed        # a fixed aria
hush figaro-bridge                        # a fresh pid-bound one
hush figaro-bridge --once --dry-run       # see what would be sent
```

As a user unit: `cp systemd/figaro-bridge.service ~/.config/systemd/user/`
then `systemctl --user enable --now figaro-bridge`.

The token comes from hush; the unit holds no credential of its own.

## Delivery

Messages are handed to figaro **immediately**, with `send --forget`, so a
second thought never waits behind the first turn. Figaro owns the queue and
does the right thing with it: a message arriving mid-turn is injected as a
*steering* node into the running turn, so "and also check X" lands while the
aria is still working.

**Replies are explicit.** Nothing an aria prints reaches the phone on its own;
it answers by running `herald say --to gluck …`.

That is the design, not a limitation of it. Tailing an aria's output sends
half-formed thinking, tool chatter and stray newlines to a phone, and makes
the aria a subject of observation rather than a correspondent. Requiring the
call means every message that arrives was meant to.

## Credentials

The token comes from the hush agent and is re-fetched on demand, because a
long-running process outlives any token handed to it at exec. On a 401 the
bridge forces `hush oauth refresh`: hush's `get` never blocks on a refresh, so
asking it twice would hand back the same dead token.
