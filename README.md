# Pilot

A single-operator control plane for a small fleet of servers: monitor, deploy,
and update every service you run — docker compose stacks, systemd units, and
static sites — with Caddy as the front door.

Pilot is a CLI (`pilot`) plus a per-host agent (`pilotd`). Deploys are pushed
over SSH; the agent owns everything after the commit point, so closing your
laptop mid-deploy cannot leave a service activated with nobody to roll it back.

## The idea

Every deploy produces an immutable, numbered release directory on the host.
Going live is one atomic symlink swap plus a single activation verb. Rollback
is the same swap in reverse. That one mechanism works identically for a
container stack, a systemd unit, and a folder of HTML — which is why
`pilot rollback` means the same thing for all three.

- **Git is desired state; the hosts are actual state.** There is no state file
  to corrupt or lose. Divergence is reported as drift.
- **The agent owns safety.** Health verification and automatic rollback run on
  the host, not in your terminal.
- **No new inbound attack surface.** The agent listens on a Unix socket; the
  CLI reaches it over your existing SSH.
- **No always-on server.** Alert rules are evaluated by each agent locally.

See [DESIGN.md](DESIGN.md) for the full design and the reasoning behind it.

## Status

Working: compose and static runtimes, Caddy route generation with per-route
network restrictions, the agent with health verification and automatic rollback,
blue-green deploys for compose, drift detection, a local alert engine, deploy
notifications, credential redaction in logs, machine-readable output on every
command, and an embedded skill for AI agents.

Not implemented, and erroring clearly when used: the systemd runtime,
`${sops:}` and `${op:}` secret schemes, rollout concurrency (serial only),
multi-host `logs --follow`, and authenticated notifier endpoints.

## Quick look

```yaml
# fleet.yaml
version: 1
hosts:
  web-1:
    address: web1.example.com
    sudo: true
```

```yaml
# services/api/service.yaml — the directory is the source, so no `source:` line
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
expose:  {domains: [api.example.com], upstream: 8080}
health:  {http: {url: "http://127.0.0.1:8080/healthz"}}
```

```
pilot init              create a fleet configuration to start from
pilot doctor            is this setup sound?
pilot bootstrap web-1   prepare the host, install the agent
pilot deploy api        build, ship, activate, verify
pilot status            what is running, and what has drifted
pilot rollback api      return to the previous release
```

## Commands

```
pilot init                 create a fleet configuration to start from
pilot doctor               check config, hosts, routing, TLS, agents, secrets in logs
pilot doctor --fix         repair what can be repaired unambiguously
pilot bootstrap <host>     prepare a host and install its agent

pilot deploy <svc>         build locally, ship, activate, verify
pilot deploy <svc> --plan  show what would change, then stop
pilot rollback <svc>       return to the previous release
pilot releases <svc>       release history, and the rollback targets

pilot status               what is running across the fleet, and what has drifted
pilot ps                   the containers and processes behind each service
pilot top                  the same, refreshing until interrupted
pilot diff                 where a host no longer matches what was deployed
pilot logs <svc>           stream logs, with credentials redacted
pilot routes               the Caddy routes Pilot manages, and any orphans

pilot agent status         each host's agent version
pilot agent upgrade        bring agents up to match this CLI
pilot skill                the agent skill; --install writes it for Claude Code
pilot upgrade              replace this binary with the latest release
```

Selectors work where they make sense: `pilot deploy @web` deploys every service
on hosts tagged `web`, and `pilot status api` narrows to one service.

## Routing

Pilot owns Caddy configuration for the services it manages, and nothing else. It
writes one file per service under `/etc/caddy/pilot.d/`, and makes exactly one
edit outside that directory — a single `import` line, added once, with a backup
taken first and restored if Caddy then rejects the result.

```yaml
expose:
  domains: [api.example.com]
  upstream: 8080            # the container's loopback port
  path: /v1/*

  timeouts: {read: 60s, write: 60s}

  allow:                    # restrict to these networks; everything else is
    - 100.64.0.0/10         # closed on rather than answered with a 403
    - "fd7a:115c:a1e0::/48"

  raw: |                    # anything not modelled, verbatim
    header /v1/* X-Robots-Tag noindex
```

A static site gets `static:` instead of `upstream:`, with SPA fallback, cache
headers, and a scoped `overlay:` for directories that survive a release.

`allow:` restricts who reaches the route *through Caddy*. It is not a substitute
for binding the service to loopback — a container published on `0.0.0.0` is
reachable on its own port whatever Caddy says, which is how a tailnet-only
service on this project's own server turned out to be answering the internet.

## Secrets

Values are referenced, never stored:

```yaml
env:
  DATABASE_URL: ${env:API_DATABASE_URL}
  SESSION_KEY:  ${cmd:security find-generic-password -s pilot/api -w}
  CLIENT_CERT:  ${file:/etc/pilot/secrets/api.pem}
```

Resolved at deploy time and written to a `0600` `.env` on the host. A reference
that cannot be resolved fails the deploy — it is never passed through as a
literal string, because shipping `${cmd:...}` as a password would be worse than
stopping. `${sops:}` and `${op:}` parse but are not implemented, and say so.

## Notifications and alerts

Each agent evaluates alert rules on its own host. There is no central server, so
they keep firing when nothing else is running.

```yaml
notifiers:
  phone:
    type: ntfy
    url: https://ntfy.sh/your-private-topic

notify_deploys: true            # default; announce every deploy and rollback

alerts:                          # host-wide; per-service rules live on a service
  - when: host.disk.used_pct > 85
    for: 10m
    cooldown: 6h
    notify: [phone]
```

Types are `ntfy`, `slack`, `discord`, `webhook`, and `command` — the last runs a local
program with the notification as JSON on stdin, which is also the easiest way to
test delivery without sending anything anywhere.

A deploy notification carries the command that reverses it:

```
PILOT: api on web-1
api deployed 0042-9f3ac1b
undo with:  pilot rollback api
```

It fires whoever ran the deploy — Pilot does not detect or care. That is the
point: a notification you did not cause is the anomaly, whatever produced it.
Delivery failures are logged by the agent and never fail a deploy.

## Rollouts and stateful services

A compose service can go live without dropping requests:

```yaml
rollout:
  strategy: blue-green     # or `recreate`, the default
  service: api             # the compose service receiving traffic
  ports: [18080, 18081]    # one host port per colour
  drain: 15s
```

The new stack starts on the other colour's port, the route moves, then the old
one drains and stops. Requests in flight finish on the release that started
them. `recreate` is simpler and briefly drops connections, which is usually fine
and is what you get without asking.

Databases and anything else Pilot should watch but never deploy:

```yaml
manage: observe
```

`pilot status` reports it, alerts fire on it, drift is detected on it — and
`pilot deploy` refuses unless you pass `--force`. That refusal is the guard that
stops a fleet-wide deploy recreating a Postgres, so it is also the flag an AI
agent is told never to use.

## Machine-readable output

Every command takes `--json`, and each declares what it does with it — a test
walks the command tree and fails if one does not, so a new command cannot
silently ignore the flag.

```
pilot status --json        per service: state, release, drift, runtime, manage
pilot doctor --json        findings with severity, scope, and hints
pilot diff --json          what diverged
pilot releases api --json  full release state, history included
pilot logs api --json      newline-delimited, one object per line
```

`logs` is NDJSON because a followed stream never ends and a document that never
closes cannot be parsed. `top` refuses `--json` rather than ignoring it, and
names `status --json` instead.

## Credentials in logs

Services log their own API keys more often than anyone expects — usually inside
a request URI. `pilot logs` replaces them with `<redacted>` by default, keeping
the label so the line stays readable:

```
uri="/api/heartbeats?api_key=<redacted>" status="201"
```

`--no-redact` shows the raw value. Detection is three layers, most certain
first: values Pilot itself supplied to the service, parameters whose *name* says
credential, and formats that can only be credentials. Entropy scanning is
deliberately not used — release IDs, commit SHAs and container IDs are all
high-entropy, so a threshold either floods the output or misses real keys.

It removes what it can **identify**, which makes logs safer to share rather than
safe. A secret logged without a recognisable label survives, which is why
`pilot doctor` also reports credentials it finds — hiding one from a single
reader does not stop the service writing it to disk on every request.

## Installing

```
brew install Gandalf-Le-Dev/tap/pilot
```

You do not install the agent yourself. `pilot bootstrap <host>` fetches the
`pilotd` published alongside your CLI's own release, verifies it against the
release checksums, and installs it under systemd. Because the agent comes from
the same release as the CLI that fetched it, protocol skew between them is
impossible rather than merely unlikely.

## Upgrading

```
pilot upgrade            # replaces this binary with the latest release
pilot upgrade --check    # exit 2 if an update exists, 0 if current
```

Homebrew installs are detected and left alone — it names `brew upgrade pilot`
rather than overwriting a file brew believes it owns.

Upgrading the CLI does not upgrade the agents on your hosts, and a newer CLI
refuses to talk to an older agent rather than guess. Keeping them in step is not
a chore you have to remember, though:

```
pilot agent status       what each host runs, and whether it matches
pilot agent upgrade      bring them all up to date
```

A deploy that meets a stale agent upgrades it and carries on, and
`pilot doctor --fix` repairs one it finds behind. `pilot bootstrap` is for
preparing a *new* host, not for updating an existing one.

## Fleet layout

```
fleet.yaml
services/
  api/
    service.yaml      the definition
    compose.yaml      what it deploys
  site/
    service.yaml
```

A service's directory is its source, so nothing needs a `source: {path: …}` line
pointing across the repository. A service built from elsewhere — a git repo, say
— declares that explicitly and the explicit source wins.

The flat form, `services/api.yaml` with `source` spelled out, still works and is
not deprecated. Both can coexist in one fleet.

`example/` holds a complete fleet exercising every feature: both runtimes,
blue-green, restricted routes, static sites, `manage: observe`, all three secret
reference forms, notifiers, and alerts. A test loads it and asserts the coverage,
so unlike a README it cannot quietly go stale.

## Using it with an AI agent

Pilot ships instructions for an agent operating a fleet — the concepts that are
not guessable from the command names, and the handful of flags an agent must
never use.

```bash
pilot skill --install     # writes .claude/skills/pilot/SKILL.md
pilot skill               # or print it, to pipe anywhere
```

It is embedded in the binary, so the guidance and the behaviour it describes are
always the same version.

**It is advisory.** Pilot does not enforce it and does not detect whether an
agent is calling — there is no permission model and no agent identity, because a
tool surface only bounds an agent that has no shell, and one with a shell calls
the CLI directly. What makes this safe enough is that a failed deploy
health-checks and rolls itself back unattended, and that every deploy is
announced through your notifiers with the command that reverses it. If you point
an AI at your infrastructure, you own what it does — the design assumes that
rather than pretending otherwise.

See **Credentials in logs** below before you point one at a real fleet.

## Building from source

```
go build ./cmd/pilot
GOOS=linux GOARCH=amd64 go build -o pilotd-linux-amd64 ./cmd/pilotd
```

A source build has no matching release to fetch from, so `bootstrap` compiles
the agent itself from the checkout the binary came from. That is the only agent
it will install for a source build: a `pilotd-linux-<arch>` found next to the
pilot binary is trusted only for a released build, where the two shipped in the
same tarball, because otherwise the filename is the only thing tying them
together.

Agents are tied to the CLI release that installed them, but keeping them in
step is not your job: a deploy that meets a stale agent upgrades it and carries
on. `pilot agent status` shows what each host runs, `pilot agent upgrade` brings
them up to date on demand, and `pilot doctor --fix` repairs one it finds behind.

There is deliberately no flag to install an agent from an arbitrary path. An
agent whose protocol and config schema nobody has checked, installed as root,
is worth less than a clear error — and the flag's one real use was hiding a CLI
that had outrun its agent, which is now caught by a failing test instead.

The agent targets Linux; the CLI runs wherever Go does.
