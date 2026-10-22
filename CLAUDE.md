# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

clipbridge — a shared clipboard (text + images + files + history) between a Linux or macOS machine and any device with a browser. One Go module (standard library only) that builds into a single self-contained binary: `main.go` at the root, packages under `internal/`, and the web UI in `web/index.html`, embedded with `//go:embed` by `web/web.go`.

## Running

```bash
go run .
go build -o clipbridge . && ./clipbridge -port 9000 -token secret -dir ~/clipbridge -history 300
```

No tests, no third-party dependencies. Clipboard tooling: on Linux `wl-clipboard` (Wayland), `xclip`, or `xsel` (xsel has no image support); on macOS the built-in `pbcopy`/`pbpaste`/`osascript`. Manual testing: run the server, open the printed URL in a browser (or drive it with `curl` against the endpoints below). The two clipboard backends are separate build targets, so check both compile: `go vet ./... && GOOS=linux go vet ./...`

Package dependencies point one way: `main` → `server` → `clipboard`, `state`, `history`, `web`. `state` and `history` import nothing internal (each keeps its own tiny `digest16`/`collapse` copy rather than sharing a util package).

## Conventions

Code comments and console output are in English. The web UI is bilingual: every user-visible string lives in the `STR` object (`ru`/`en`) in `web/index.html` and is applied via `tr()` / `data-i18n` (plus `data-i18n-placeholder`, `data-i18n-alt`) attributes — when adding UI text, add it to both languages, never hardcode it.

"linux" in log lines (`[linux -> web]`), `state.OriginHost`, and UI labels means "the machine running the server", which may be a Mac. The `origin` values `"linux"`/`"web"` are part of the client protocol — the page checks `origin === 'linux'`.

## Architecture

- **`main.go`** — flags, `run()` (returns errors instead of exiting), wires `clipboard.New` + `state.New` + `history.Open` into `server.New`. Ctrl+C/SIGTERM cancels a root context: watchers stop, the macOS helper is killed, and it is the server's `BaseContext`, so pending long polls return at once before `Shutdown`.
- **`internal/clipboard/clipboard.go`** — the `Clipboard` interface (formats are MIME types: `image/*`, `text/uri-list` / `x-special/gnome-copied-files` for copied files) and the `cmdOutput`/`cmdInput` subprocess helpers. `cmdInput` must not capture output: `xclip -i` and `wl-copy` fork a child that holds inherited pipes open.
- **`internal/clipboard/clipboard_unix.go`** (`!darwin`) — `toolClipboard`, a table of argv per operation for `wl-clipboard`/`xclip`/`xsel`, picked in that order by `New`.
- **`internal/clipboard/clipboard_darwin.go`** — `macClipboard`. Text via `pbcopy`/`pbpaste` with `LC_CTYPE=UTF-8` (otherwise non-ASCII is mangled); images and Finder file copies via JXA scripts (`osascript -l JavaScript`). Spawning osascript costs ~0.25s, so one long-running JXA helper watches `NSPasteboard.changeCount` and streams `{n, types}` JSON lines; image/file reads are cached per change count, so an idle pasteboard costs nothing. `public.tiff` is served converted to PNG; `public.file-url` wins over images (Finder also puts the file icon on the pasteboard). Our own writes hold the `writing` mutex and synchronously `refresh()` afterwards, and change counts only move forward — otherwise the poller sees stale pre-write types and re-announces the cached previous image as a fresh copy (echo + bogus history entries). The helper also exits by itself once its parent dies (`getppid() == 1`).
- **`internal/state`** — `State`: the current clipboard snapshot (text or image), files currently on the system clipboard (`ClipFile`, for "Grab"), share-folder file list, and a monotonically increasing `version`. Every mutation calls `bumpLocked()`, which closes and replaces the `changed` channel; `WaitForChange(ctx, …)` is what long-polling clients block on. Setters return whether anything changed — that is the echo-loop guard.
- **`internal/history`** — `History`, a persistent copy log under `<share_dir>/.clipbridge/`: text entries in `history.json`, images as files in `img/` named by SHA1 digest (deduplicated; deleted only when no entry references them). Thread-safe via its own mutex; `onChange` (wired to `State.NoteHistory`) bumps `history_rev`.
- **`internal/server/watch.go`** — `syncClipboard()` polled every 0.5s pushes clipboard changes into `State`/`History`; the share folder is rescanned every 1s.
- **`internal/server/server.go`** — `Server` (`New`, `Watch(ctx)`, `Handler()`); routes on a Go 1.22+ `ServeMux` with method patterns: `GET /clip` (long-poll, up to 25s), `GET /history`, `GET /history/image/{id}`, `GET /image/{id}`, `GET /file/{name}`, `POST /clip` (text from browser), `POST /upload` (files/pasted images streamed to disk; `clipboard=1` also puts an image into the system clipboard), `POST /grab` (copy a file from the system clipboard into the share folder), `POST /restore` (push a history entry back into a clipboard), `POST /history/clear`. Errors are JSON `{"error": …}`, including the catch-all 404.
- **`internal/server/files.go`** — share-folder helpers (`scanShareDir`, `safeTarget` for timestamped collision-free names, `copyIntoShare`). **`internal/netaddr`** — which LAN addresses to print.
- **`web/index.html`** — the whole browser client. The JS long-polls `/clip` with its known `version`; a separate `history_rev` counter inside the response tells it when to re-fetch `/history`.

Data flow is symmetric: system → browser goes clipboard poller → `State` → long-poll response; browser → system goes HTTP POST → `Clipboard` write → `State` (which the poller then sees as unchanged, avoiding echo loops — dedup is done by comparing text equality and image SHA1 digests in `State.SetText`/`SetImage`). This means images must round-trip byte-for-byte through the clipboard tool, or every browser paste echoes back.

Auth is a single optional `-token`: GET endpoints and `/upload` take it as a `token` query param (`withToken` middleware), the JSON POST endpoints as a `token` field (`decodeRequest`); compared in constant time. Only `/` is open. There is no HTTPS — this is intended for a trusted local network.
