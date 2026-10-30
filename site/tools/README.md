# Site tools

Not published: the Pages workflow leaves `site/tools/` out.

- `capture.mjs`: the screenshots in `../assets/shots/` and the social card `../assets/og-image.png`.
- `check.mjs`: checks the site in a browser and writes full-page screenshots.

```sh
cd site/tools
npm ci
npx playwright-core install chromium   # or set CHROMIUM_PATH to a Chromium binary
```

## Screenshots

They come from a throwaway test copy of İndirgen with an empty data folder,
never from a real installation. An empty `PATH` makes it install every engine
into its own data folder, and its own `HOME` keeps your user name, home
folder and Firefox profile out of the pictures. ffmpeg's archive needs `tar`
and `xz`:

```sh
T=/tmp/indirgen-shots && mkdir -p $T/home $T/bin
for x in tar xz; do ln -s "$(command -v $x)" $T/bin/$x; done
go build -o $T/indirgen ../..
env -i HOME=$T/home PATH=$T/bin LANG=C.UTF-8 TZ=Europe/Istanbul \
  $T/indirgen -data $T/data -library $T/İndirgen -listen 127.0.0.1:8898
```

In another terminal:

```sh
node capture.mjs seed        # 9 Blender open movies through the UI, then Elephants Dream slowly
node capture.mjs shots       # 9 views × TR/EN × dark/light → ../assets/shots/*.webp
node capture.mjs og          # ../assets/og-image.png from og.html
```

`seed` sets a 1 MB/s limit before the last film. İndirgen divides it among
`concurrency` jobs, so with `"concurrency": 16` in `$T/data/config.json` (set
it while the copy is stopped) Elephants Dream runs at 64 KB/s for about 40
minutes, long enough for every shot to show a download in progress. If it
finishes, change `SLOW` in capture.mjs to another Blender open movie and run
`node capture.mjs seed slow`.
`node capture.mjs shots pairing,devices` retakes only those views.

Only Blender open movies (CC BY) are downloaded. The test copy listens on
127.0.0.1 only, so it has no network access, pairing or signed-in devices:
for those views the script answers `/api/connect`, `/api/pair` and
`/api/sessions` itself with an example home network (192.168.1.x, a fake
token, generic device names). The QR code in them is drawn by the app's own
`qr.go`.

The PNGs become WebP through ImageMagick (`magick`, `-strip`: no EXIF or
other metadata), 1600 px wide for the computer and 780 px for the phone.

## Checks

```sh
node check.mjs [out-dir] [--online]
```

1440, 390 and 320 px, TR and EN, dark and light: console errors, failed
requests, in-page links, images and their alt text, sideways scrolling, WCAG
AA contrast, keyboard focus, the install tabs, copying, remembered
preferences, blocked storage, the page without JavaScript, reduced motion,
the keys of `tr.js` against `index.html`, metadata in images and the 3 MB
limit. `--online` also opens every outside link.
