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

Upgrading the CLI does not upgrade the agents already on your hosts. A newer
CLI refuses to talk to an older agent rather than guess, so run
`pilot bootstrap <host>` afterwards to bring each one up to match.

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

There is deliberately no flag to install an agent from an arbitrary path. An
agent whose protocol and config schema nobody has checked, installed as root,
is worth less than a clear error — and the flag's one real use was hiding a CLI
that had outrun its agent, which is now caught by a failing test instead.

The agent targets Linux; the CLI runs wherever Go does.
