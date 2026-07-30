# Example fleet

A complete Pilot fleet exercising every feature, kept honest by a test in
`internal/config` that loads it and asserts zero diagnostics. Documentation that
has drifted is worse than none, so this cannot rot without the build failing.

```
fleet.yaml              hosts, Caddy paths, notifiers, host-wide alerts
services/
  api/                  compose · blue-green · secrets · restricted route
    service.yaml
    compose.yaml
  site/                 static · built from git · SPA · cache headers
    service.yaml
  db/                   manage: observe — watched, never deployed
    service.yaml
    compose.yaml
```

Each service lives in its own directory, and that directory is its source — so
nothing needs a `source: {path: …}` line pointing across the repository. A
service built from elsewhere, like `site`, says so explicitly and that wins.

The flat `services/<name>.yaml` layout still works; it simply requires `source`
to be spelled out.

Nothing here is deployable as-is: the images, domains, and repositories are
invented. Copy the shape, not the values.
