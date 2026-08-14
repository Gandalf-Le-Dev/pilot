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

The logo is an original drawing; the fold style nods to the classic
hand-drawn paper-plane icons.

## The dashboard screenshot

`dashdemo/` serves the real dashboard — server, views, charts — with
example-fleet data (fixed seed, so re-captures diff only where reality
changed). Capture with headless Chrome:

```
go run ./docs/readme/dashdemo &
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --headless=new --screenshot=docs/readme/dashboard.png \
  --window-size=1240,1660 --hide-scrollbars \
  --virtual-time-budget=6000 http://127.0.0.1:5481/
```

Trim trailing space if the page got shorter:
`ffmpeg -i dashboard.png -vf "crop=1240:<height>:0:0" …`
