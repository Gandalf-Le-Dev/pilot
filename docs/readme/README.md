# README assets

The terminal captures are rendered by Pilot's own terminal renderer, fed with
the example fleet's vocabulary — no real fleet is photographed. `demo/` drives
the same render package the CLI uses, and the module compiles it, so a capture
can go stale but cannot drift from the real output shapes.

Regenerate from the repo root with [vhs](https://github.com/charmbracelet/vhs):

```
vhs docs/readme/status.tape
vhs docs/readme/doctor.tape
vhs docs/readme/deploy.tape
```

Each tape writes `shots/<name>.png`; if a run leaves no screenshot (vhs is
flaky about the Screenshot command), extract the last frame of the scratch gif
it always produces:

```
ffmpeg -sseof -0.5 -i vhs-scratch-<name>.gif -frames:v 1 docs/readme/<name>.png
```

The logo is an original drawing; the trail-and-fold style nods to the classic
hand-drawn paper-plane icons.
