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
credential for this machine, and cannot push anything into an aria — the trust
arrow points outward only. It is also why herald could ship before figaro ever
runs on spain.

herald is a dependency of this program; the dependency does not run the other
way. Herald knows nothing about figaro, and that is deliberate.

## Roles

Needs both herald roles — `say` and `inbox` — because it is the one caller
that genuinely reads your replies. Compare `kcal-notify`, which holds `say`
alone and therefore cannot.

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

At-least-once. A message is acknowledged only after the reply is out, so a
crash mid-turn replays it rather than losing it.
