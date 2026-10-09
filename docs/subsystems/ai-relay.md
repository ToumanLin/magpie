# AI Relay: Mag, Pie and the hub

AI Relay lets a small group — a lab, a family — share AI resources that one
person looks after. It is this repository built as two programs:

- **Pie**, the server: providers, accounts, sign-in plugins and routing, as
  magpie has them, and the **hub gateway** members call.
- **Mag**, the member's client: it wires the agents on a member's computer
  to its **local gateway**, which sends every request on to the hub with
  the member's device key. It keeps no upstream account.

A member who installs nothing calls the hub directly with a device key made
on the hub's page.

```
agent → [Mag + local gateway] → [hub gateway + Pie] → vendor
bare API client ───────────────↗
```

Nothing here translates protocols or reaches a vendor in a new way: Mag's
connection is a [Remote magpie provider](../../internal/provider/remote_magpie.go),
a member's device is a gateway key, and a request is authenticated, held to
its models, routed and counted by the code that does so for any gateway key
([gateway routing](gateway-routing.md), [providers and accounts](providers-accounts.md)).

## Responsibilities and sources of truth

| Part | Responsibility | Source |
| --- | --- | --- |
| Mode | Which program a process is: `client`, `server`, or none for magpie. Built in with `-ldflags "-X github.com/yetone/magpie/internal/mode.Default=client"`, or `AIRELAY_MODE` for a plain build. Never read from a settings file. Each mode has its own data folder (`airelay-mag`, `airelay-pie`; `data-mag`, `data-pie` when portable). | [`internal/mode`](../../internal/mode/mode.go), [`appdir`](../../internal/appdir/appdir.go) |
| Members | Members, groups (which models), plans (what use costs and how much it may be), invitations, passwords and web sessions, in `relay.json`. No code, password, session or key is kept as given. | [`internal/relay`](../../internal/relay/relay.go), [`auth.go`](../../internal/relay/auth.go) |
| Devices | A device is `access.Key` with `Member` set, kept by its SHA-256 (`Hash`). `access.Authenticate` asks `access.MemberOf` for the member's models, limit and concurrency **on every request**; a member with no model gets `access.NoModels`, which matches nothing. | [`access.go`](../../internal/access/access.go), [`resolve.go`](../../internal/relay/resolve.go) |
| Hub guard | In server mode `lanGuard` is `hubGuard`: every request for a model needs an enabled key, from loopback too. Open paths: `GET /`, `/api/hello`, preflights, and `/relay/*`, which signs its callers in itself. A member's device is refused the account reports (`memberOnly`), the MCP proxy and the Codex passthrough, and cannot name an account (`X-Magpie-Account` is dropped). | [`gateway/hub.go`](../../internal/gateway/hub.go), [`lan.go`](../../internal/gateway/lan.go) |
| Limits | `budget.ReserveFor` checks a key's own limit and its member's under one lock, then reserves against both. `keyLimited` runs, in order: the member's turn (`memberLanes`, a FIFO queue), the reservation, `relay.Begin`. | [`budget.go`](../../internal/budget/budget.go), [`key_limit.go`](../../internal/gateway/key_limit.go) |
| Ledger | `usage.jsonl`. `appendUsage` calls `relay.Stamp`, which writes the member, the plan, its charge and `Settle` on each record. The member's page and the administrator's totals read the same rows (`StatementOf`, `Overview`). | [`ledger.go`](../../internal/relay/ledger.go), [`usage.go`](../../internal/usage/usage.go) |
| Member API and page | `/relay/` on the hub gateway: sign-in, standing, models, devices, usage. Every answer is of the caller alone; no route takes a member's id from the request. | [`http.go`](../../internal/relay/http.go), [`portal.html`](../../internal/relay/portal.html) |
| Client | `hubclient` connects (invitation, password or device key), saves the hub as provider `hub`, and asks it for the member's standing and statement. | [`internal/hubclient`](../../internal/hubclient/hubclient.go) |
| Pages | `bootPrefs.relay` picks the tabs: Mag has Hub, Agents, Usage, Sessions, Library; Pie has Providers, Gateway, Routing, Members, Usage, Plugins. `modeGuard` refuses the other program's `/api` routes with 403 whatever asks. | [`gui/relay.go`](../../internal/gui/relay.go), [`assets/relay.js`](../../internal/gui/assets/relay.js) |
| Commands | `mag hub …`, `pie relay …`, `pie serve --listen`; a command that is the other program's says so. | [`relay_cli.go`](../../relay_cli.go) |

## A request through the hub

1. `hubGuard` authenticates the key. For a device, `relay.memberOf` builds
   the identity from the member's groups, plan and own settings as they are
   now. A member turned off or removed, a device taken away, or a
   `relay.json` that does not parse refuses the request.
2. `keyLimited` reads the body for the model, waits for one of the member's
   slots (`Concurrency` running, `Queue` waiting, `relay.QueueWait` at
   most; else 429 with `Retry-After`), then reserves against the limits
   (429 with `X-Magpie-Limit-Reset` when spent), then writes the request to
   `relay-open.json`.
3. Routing serves it as it serves any gateway key held to some models
   (`key_models.go`): a model the member's groups do not give is 403 before
   any provider is asked.
4. The call's record is stamped and appended; the reservation is released
   and the request leaves `relay-open.json` when the handler returns.

## How a reservation ends (`usage.Record.Settle`)

| Settle | When | Counted against the limit, and charged |
| --- | --- | --- |
| `settled` | the vendor reported usage, on success or on a reply cut short | the reported tokens |
| `estimated` | a 2xx with no usage reported, or a reply cut off (499) after a vendor had the request and reported nothing | the tokens reserved (`Est`); the record has `usage_unknown` |
| `released` | refused by the hub, or the vendor answered an error | nothing |
| `interrupted` | the hub stopped mid-request; written by `relay.Recover` when it next starts | the tokens reserved (`Est`) |

The reservation is the request body's size in tokens (4 bytes a token) plus
the mean output per call this window, so requests arriving together can
overshoot a limit by about one call's output, never by a whole request
each. Unknown usage is never counted as zero.

## Plans

- **Metered**: prices for a million tokens (input, output, cache read,
  cache write). Each record carries its `Charge`. `Budget` caps the charge
  in a window (`access.Limit.Charge`).
- **Period**: a price for each calendar window (day, week, month) with
  tokens included, which is the member's token limit. A statement's `Fee`
  is the price times the windows it touches.

A member's own `Limit` replaces the plan's. Plans are given by the
administrator; nothing is paid through the hub.

## Sessions are the caller's

On a hub, everything remembered by session or conversation is keyed by the
member too (`scopeOf`, `scoped`): affinity sticks, routing rules' turn
decisions, a sealed Codex lead, and a Claude subscription run's owner. Two
members who send the same session id or the same first message share no
state. A key of the hub's own is scoped by its key id. Off a hub the keys
are as they were.

## Constraints and failure behavior

- A tab left out is not access control. `modeGuard` and `hubGuard` are; the
  tests call the handlers directly.
- `provider.Accounts()` returns nothing in client mode, `provider.Save`
  refuses anything but a Remote magpie, and the catalog skips a stored
  provider that is not one: Mag reads no agent's sign-in and keeps no
  upstream key. `Bundle.ForClient` applies the same to backups, on export
  and on import.
- Pie wires no agent and writes into no agent's files on the computer it
  runs on, and lists none (`run` in `main.go`, `state` in `gui/api.go`). It
  still looks whether any is installed, for gateway mode (`agentsHere`).
- Mag's local gateway has a default port of its own, `settings.ClientPort`
  (3525): a magpie already serving 3425 on the member's computer is not
  taken for it.
- Mode builds never update themselves and send no usage statistics
  (`update.Released`).
- The hub speaks plain HTTP. `pie serve` listens on loopback unless
  `--listen` or `MAGPIE_ADDR` says otherwise; beyond a trusted network it
  belongs behind a VPN or an HTTPS reverse proxy, with `MAGPIE_PUBLIC_URL`
  set. `AIRELAY_LOCAL_OPEN=1` lets the hub's own computer in without a key.
  Wrong sign-in tries are counted by the address that connected, or, with
  `MAGPIE_TRUST_PROXY=1`, by the client a proxy on the hub's computer
  names in `X-Forwarded-For`.
- No request or reply text is written by this subsystem. The request
  archive and the Recent-calls bodies are magpie's own features and stay
  off unless the administrator turns them on.
- Not in a backup: `relay.json` and `usage.jsonl` are copied by hand.
- One hub process serves. There is no cross-process lock on `relay.json`
  beyond the reload each write does, as `caller-keys.json` has none.
  `relay-open.json` is written whole at each request's start and end: fit
  for a small group, not for hundreds of requests a second.

## Verification

```sh
go test -tags nogui ./internal/relay ./internal/hubclient ./internal/access ./internal/budget ./internal/mode
go test -tags nogui ./internal/gateway -run 'TestHub|TestScopeOnlyOnAHub'
go test -tags nogui ./internal/gui -run 'TestModeGuard|TestRelayAdmin'
go test -tags nogui ./internal/backup -run TestClientBundle
```

`TestHub*` stand a hub up on a real listener with stand-in vendors: two
members' models and statements, sign-in, streaming with a tool call,
unknown usage, failure, limits under load, reservations, concurrency and
queueing, a caller that goes away, a vendor that hangs up, a restart, and
sessions kept apart, and the member's models holding on every API path
the hub serves. None asks a real vendor.
