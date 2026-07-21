# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

clipbridge — a shared clipboard (text + images + files + history) between a Linux machine and any device with a browser. The entire project is one file, `main.py`: server, clipboard backend, and the web UI (HTML/CSS/JS embedded in the `PAGE` string).

## Running

```bash
python3 main.py
python3 main.py --port 9000 --token secret --dir ~/clipbridge --history 300
```

No build, no tests, no third-party dependencies — Python stdlib only. The only system requirement is a clipboard tool: `wl-clipboard` (Wayland), `xclip`, or `xsel` (xsel has no image support). Manual testing: run the server, open the printed URL in a browser.

## Conventions

Code comments, docstrings, and console output are in English. The web UI is bilingual: every user-visible string lives in the `STR` dictionary (`ru`/`en`) inside `PAGE` and is applied via `tr()` / `data-i18n` attributes — when adding UI text, add it to both languages, never hardcode it.

## Architecture

Everything lives in `main.py`, organized in sections marked by `# ---- <name>` comment dividers:

- **`Backend`** — wrapper over `wl-clipboard`/`xclip`/`xsel` subprocess calls; auto-detects which is installed at startup. All Linux clipboard I/O goes through it.
- **`History`** — persistent copy log under `<share_dir>/.clipbridge/`: text entries in `history.json`, images as files in `img/` named by SHA1 digest (deduplicated; deleted only when no entry references them). Thread-safe via its own lock.
- **`State`** — the current clipboard snapshot (text or image), share-folder file list, and a monotonically increasing `version` counter. Built around a `threading.Condition`: every mutation calls `_bump()` which notifies waiters; `wait_for_change()` is what long-polling clients block on.
- **Watcher threads** — `watch_clipboard()` polls the Linux clipboard every 0.5s and pushes changes into `State`/`History`; `watch_share_dir()` rescans the shared folder every 1s.
- **`Handler`** — `BaseHTTPRequestHandler` on `ThreadingHTTPServer`. Key endpoints: `GET /clip` (long-poll, blocks up to 25s on `State.wait_for_change`), `POST /clip` (text from browser), `POST /upload` (files/pasted images, streamed to disk), `POST /restore` (push a history entry back into a clipboard), `GET /file/...` and `/image/...` / `/history/image/...` for downloads.
- **`PAGE`** — the whole browser client as one raw string. The JS long-polls `/clip` with its known `version`; a separate `history_rev` counter inside the response tells it when to re-fetch `/history`.

Data flow is symmetric: Linux → browser goes clipboard poller → `State` → long-poll response; browser → Linux goes HTTP POST → `Backend` write → `State` (which the poller then sees as unchanged, avoiding echo loops — dedup is done by comparing text equality and image SHA1 digests in `State.set_text`/`set_image`).

Auth is a single optional `--token` compared against a `token` query param or JSON field on every endpoint except `/`. There is no HTTPS — this is intended for a trusted local network.
