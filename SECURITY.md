# Security

İndirgen is meant to run for one person, on their own computer or server, reachable only locally or through a VPN.

## Reporting a vulnerability

Please report security problems privately through GitHub: **Security → Report a vulnerability** on this repository. Do not open a public issue for them.

Include the version (`indirgen version`), how it is installed (native or Docker) and the steps to reproduce.

## What İndirgen protects

- Without network access (the default), it listens on `127.0.0.1` only.
- With network access on, every request from another device needs the access token. Each request is also checked for cross-site origins (CSRF) and for a Host name it does not know (DNS rebinding).
- File serving and deletion are confined to the archive and the temporary folder.
- Engine and self-updates are installed only after their SHA-256 checksum matches the release.

## Your data

Settings, the access token, the Telegram bot key, cookies and the download history live in the data folder (`~/.local/share/indirgen` or Docker's `./data`), never in this repository. A backup ZIP contains all of them: keep it private.
