# UI tests

Browser tests of İndirgen, run by hand against a throwaway test copy. Not in
CI: they need the real engines, the network and a few minutes.

| Script | What it checks |
|---|---|
| `dialog.mjs` | The subscribe dialog at 1440 and 390 px, TR/EN, dark/light: focus on open, Tab stays inside, Esc closes and gives focus back, the chosen values reach `POST /api/subs` (answered by the script, so nothing is subscribed). |
| `uifix.mjs` | At 1440, 390 and 320 px: the theme buttons are SVG icons of one size with names; a running download's figures stay on one line with the speed and time left. Starts Sintel under a 1 MB/s limit if nothing is running, then stops it and restores the limit. |
| `e2e.mjs` | Real subscriptions: "only what comes next" on the Blender Studio channel downloads nothing and shows "Existing items marked…"; "Check now" downloads nothing; a normal subscription to Blender Studio's open movies playlist downloads and counts what is not marked. |

Every script exits 1 on a failed expectation and leaves its screenshots in
`$UI_OUT` (a new temp folder by default).

## The test copy

Never your real installation. The scripts refuse to run unless the address
is 127.0.0.1 or localhost on a port other than 8899, and the copy's data and
archive folders lie outside your home folder. Start one with its own empty
folders, an empty `PATH` (so it installs its own engines instead of using
yours) and its own `HOME` (so it does not see your Firefox profile); ffmpeg's
archive needs `tar` and `xz`:

```sh
T=/tmp/indirgen-ui && mkdir -p $T/home $T/bin
for x in tar xz; do ln -s "$(command -v $x)" $T/bin/$x; done
go build -o $T/indirgen .
env -i HOME=$T/home PATH=$T/bin LANG=C.UTF-8 TZ=Europe/Istanbul \
  $T/indirgen -data $T/data -library $T/İndirgen -listen 127.0.0.1:8898
```

Wait until the engines are ready (the log says "motor hazır" four times).
`e2e.mjs` expects a fresh copy: on one where the playlist is already
downloaded, step 3 finds nothing new.

## Running

```sh
cd tools/ui-tests
npm ci
npx playwright-core install chromium   # or set CHROMIUM_PATH to a Chromium binary
node dialog.mjs
node uifix.mjs
node e2e.mjs
```

`INDIRGEN_URL` points elsewhere than `http://127.0.0.1:8898`; `UI_OUT` sets
the screenshot folder.
