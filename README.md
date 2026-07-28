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

Working: compose and static runtimes, Caddy route generation, the agent with
health verification and automatic rollback, drift detection, a local alert
engine, and blue-green deploys for compose.

Not implemented, and erroring clearly when used: the systemd runtime,
`${sops:}` and `${op:}` secret schemes, rollout concurrency (serial only), and
multi-host `logs --follow`.

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
# services/api.yaml
name: api
runtime: compose
hosts: [web-1]
source:  {repo: "git@github.com:me/api.git", ref: main}
build:   {image: "ghcr.io/me/api", dockerfile: Dockerfile}
compose: {file: deploy/compose.yaml}
expose:  {domains: [api.example.com], upstream: 8080}
health:  {http: {url: "http://127.0.0.1:8080/healthz"}}
```

```
pilot doctor            is this setup sound?
pilot bootstrap web-1   prepare the host, install the agent
pilot deploy api        build, ship, activate, verify
pilot status            what is running, and what has drifted
pilot rollback api      return to the previous release
```

## Building

```
go build ./cmd/pilot
GOOS=linux GOARCH=amd64 go build ./cmd/pilotd
```

The agent targets Linux; the CLI runs wherever Go does.
