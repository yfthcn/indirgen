# Changelog

## 1.0.1 · 2026-10-30

- **Subscribing asks how**: the Subscribe button opens a small dialog with the check interval, how many of the newest items each check looks at, and "Don't download what is there now, only what comes next". With that ticked, the first check downloads nothing and marks the newest items as seen; later checks download only what is new. The subscription says "Existing items marked; only new ones will be downloaded from now on." and does not count them as new.
- **Theme icons**: the auto, dark and light buttons in the settings header are drawn as icons, no longer as characters that some fonts showed in part.
- **Running downloads on a phone**: their speed and time left stay on one line; less important figures give way when the screen is narrow.
- **No macOS binary**: the native install is Linux only (amd64, arm64). Windows and macOS run İndirgen in Docker Desktop; the README has the steps (experimental, not tested there yet). The install script on a Mac now points there.

## 1.0.0 · 2026-10-29

First release: a self-hosted media downloader for one person, in one Go binary.

- **yt-dlp and gallery-dl behind one interface**: paste links, see what they are, pick quality or posts, download. İndirgen picks the engine and falls back to the other; "already downloaded" is recognised before downloading again.
- **Engines kept current**: yt-dlp, gallery-dl, ffmpeg and Deno install themselves, update to the latest stable release (yt-dlp nightly on request), are checksum-verified and test-run first, and can be rolled back.
- **Queue and archive**: live progress, per-site parallelism, retries, night hours, speed limit, low-disk guard; browse, search, play, ZIP and delete downloads in the browser.
- **Subscriptions and Instagram**: channels, playlists, profiles and galleries checked on a schedule; Instagram sections or hand-picked posts.
- **From anywhere**: a Telegram bot, an iPhone Shortcut, a browser extension (Chrome/Edge, Firefox), and the phone as a home-screen app, paired by QR or a short code.
- **Private by default**: listens on this computer only; other devices need the token and are listed under Connected devices, where each can be signed out. No account, no telemetry.
- **Logins**: Firefox's logins used directly, or an imported cookies.txt, one file per site.
- **Install**: a user systemd service on Linux without sudo, or Docker (amd64, arm64); self-update checked against SHA256SUMS; backup and restore in one ZIP.
