# clipbridge

A shared clipboard (text + images + files + history) between a Linux (or macOS) machine and any device with a browser. A single self-contained Go binary, standard library only — nothing to install on the second device.

## How it works

A small HTTP server runs on the Linux machine (or a Mac — see below; "Linux" below means whichever machine runs the server). On any other device (Windows, Mac, a phone — anything with a browser) you open a page, and then:

- **Text** — syncs both ways. Copy on Linux — it shows up in the browser; paste in the browser and hit "Send" (or Ctrl+Enter) — it lands in the Linux clipboard.
- **Images** — an image copied on Linux is shown in the browser as a preview (right click → "Copy image"). An image pasted into the browser with Ctrl+V goes straight into the Linux system clipboard.
- **Files** — a shared folder. Drag&drop into the browser window puts the file on the Linux side; everything in the folder is listed on the page and downloadable in one click. The other way round: copy a file in a Linux file manager and the page shows it with a "Grab" button, which copies it into the shared folder to download.
- **History** — everything copied (text and images) is written to disk and survives restarts. Any entry can be restored to the Linux clipboard or copied to the current device's clipboard.

## Installation

You need Go (only to build) and one of the clipboard tools:

```bash
sudo apt install wl-clipboard   # Wayland
sudo apt install xclip          # X11
sudo apt install xsel           # X11, no image support
```

That's it — no third-party dependencies.

**macOS** works too, with nothing extra to install: the built-in `pbcopy`/`pbpaste` handle text, and `osascript` (JavaScript for Automation) handles images and files copied in Finder. Images copied as TIFF (most macOS apps) are served to the browser as PNG.

## Usage

```bash
go build -o clipbridge .   # once; produces a single static binary
./clipbridge               # or just: go run .
```

The server prints an address like `http://192.168.x.x:8765/` — open it in a browser on the other device.

All options:

```bash
./clipbridge -port 9000 -token secret -dir ~/clipbridge -history 300
```

| Option | Default | What it does |
|---|---|---|
| `-host` | `0.0.0.0` | Address the server listens on |
| `-port` | `8765` | Port |
| `-token` | *(empty)* | Simple password; appended to the URL as `?token=...` |
| `-dir` | `~/clipbridge` | Shared folder for files |
| `-history` | `200` | How many history entries to keep (`0` — disable) |

History is stored in `<shared folder>/.clipbridge/`: text in `history.json`, images as files in `img/`.

## Security

This is a tool for a trusted local network. There is no HTTPS, and the token travels in the URL in plain text. Do not expose the port to the internet; for remote access use a VPN (WireGuard, Tailscale) or an SSH tunnel.

## Under the hood

A small Go program (`package main`, one binary):

```
main.go                 flags, startup, shutdown
internal/clipboard/     the system clipboard behind one interface:
                        wl-clipboard / xclip / xsel on Linux, the pasteboard on macOS
internal/state/         the current clipboard snapshot that browsers long-poll
internal/history/       the persistent copy history
internal/server/        HTTP API, clipboard and shared-folder pollers
internal/netaddr/       which LAN addresses to print
web/index.html          the web page, embedded into the binary with //go:embed
```

How it works:

- a background goroutine polls the Linux clipboard every 0.5 s via `xclip`/`wl-paste`; on macOS a long-running `osascript` helper watches the pasteboard's change count, so an idle clipboard costs nothing;
- the browser keeps a long-poll request to `/clip` and receives changes instantly;
- echo loops are suppressed by comparing text and image SHA1 digests;
- the web page (dark/light theme auto-following the system, Russian/English interface with a toggle in the header) is embedded in the binary.

