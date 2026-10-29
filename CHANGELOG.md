# Changelog

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
