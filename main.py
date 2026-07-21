#!/usr/bin/env python3
"""
clipbridge — shared clipboard (text + images + files + history)
between a Linux laptop and any device with a browser.

Run on the LINUX machine:
    python3 clipbridge.py
    python3 clipbridge.py --port 9000 --token secret --dir ~/clipbridge --history 300

Open the printed address in a browser on the other device. Nothing to install there.

Features:
  text      linux <-> browser, automatic in both directions
  images    linux  -> browser as a preview (right click -> copy image)
            browser -> linux via Ctrl+V, goes straight into the system clipboard
  files     shared folder: drag&drop in the browser puts a file on the linux side,
            the whole folder is visible and downloadable from both ends
  history   everything copied (text/images) is written to disk and survives
            restarts; any entry can be restored to the linux clipboard or this PC's

Dependencies: standard library only + xclip (X11) or wl-clipboard (Wayland).
    sudo apt install xclip        # or: sudo apt install wl-clipboard
"""

import argparse
import hashlib
import json
import mimetypes
import os
import shutil
import subprocess
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import quote, unquote

POLL_INTERVAL = 0.5
LONGPOLL_TIMEOUT = 25.0
MAX_UPLOAD = 200 * 1024 * 1024  # 200 MB
IMAGE_TYPES = ("image/png", "image/jpeg", "image/gif", "image/webp")


# ---------------------------------------------------------------- backend

class Backend:
    """Wrapper around xclip / xsel / wl-clipboard."""

    def __init__(self):
        if shutil.which("wl-paste") and shutil.which("wl-copy"):
            self.kind = "wayland"
        elif shutil.which("xclip"):
            self.kind = "xclip"
        elif shutil.which("xsel"):
            self.kind = "xsel"  # no image support
        else:
            raise SystemExit(
                "Neither xclip, xsel, nor wl-clipboard found.\n"
                "Install one of them: sudo apt install xclip"
            )

    @property
    def supports_images(self):
        return self.kind in ("wayland", "xclip")

    def _run(self, cmd, stdin=None, timeout=3):
        return subprocess.run(cmd, input=stdin, capture_output=True, timeout=timeout)

    def targets(self):
        try:
            if self.kind == "wayland":
                out = self._run(["wl-paste", "--list-types"]).stdout
            elif self.kind == "xclip":
                out = self._run(["xclip", "-selection", "clipboard",
                                 "-t", "TARGETS", "-o"]).stdout
            else:
                return ["text/plain"]
            return [line.strip() for line in out.decode("utf-8", "replace").splitlines()
                    if line.strip()]
        except Exception:
            return []

    def read_text(self):
        try:
            if self.kind == "wayland":
                cmd = ["wl-paste", "--no-newline"]
            elif self.kind == "xclip":
                cmd = ["xclip", "-selection", "clipboard", "-o"]
            else:
                cmd = ["xsel", "--clipboard", "--output"]
            return self._run(cmd).stdout.decode("utf-8", "replace")
        except Exception:
            return ""

    def write_text(self, text):
        try:
            if self.kind == "wayland":
                cmd = ["wl-copy"]
            elif self.kind == "xclip":
                cmd = ["xclip", "-selection", "clipboard", "-i"]
            else:
                cmd = ["xsel", "--clipboard", "--input"]
            self._run(cmd, stdin=text.encode("utf-8"))
        except Exception as exc:
            print("Failed to write text to clipboard:", exc)

    def read_image(self, mime):
        try:
            if self.kind == "wayland":
                cmd = ["wl-paste", "--type", mime]
            else:
                cmd = ["xclip", "-selection", "clipboard", "-t", mime, "-o"]
            return self._run(cmd, timeout=6).stdout
        except Exception:
            return b""

    def write_image(self, data, mime):
        if not self.supports_images:
            return
        try:
            if self.kind == "wayland":
                cmd = ["wl-copy", "--type", mime]
            else:
                cmd = ["xclip", "-selection", "clipboard", "-t", mime, "-i"]
            self._run(cmd, stdin=data, timeout=6)
        except Exception as exc:
            print("Failed to write image to clipboard:", exc)


backend = None  # created in main


# ---------------------------------------------------------------- history

class History:
    """Log of copied items, persisted to disk.

    Text is stored right in the json, images as files in .clipbridge/img/.
    Shared-folder files are not logged (they are already in place).
    """

    def __init__(self, base_dir, limit, on_change):
        self.limit = limit
        self.on_change = on_change
        self.dir = os.path.join(base_dir, ".clipbridge")
        self.img_dir = os.path.join(self.dir, "img")
        os.makedirs(self.img_dir, exist_ok=True)
        self.path = os.path.join(self.dir, "history.json")
        self.lock = threading.Lock()
        self.entries = self._load()

    # --- disk

    def _load(self):
        try:
            with open(self.path, encoding="utf-8") as fh:
                data = json.load(fh)
            return data if isinstance(data, list) else []
        except Exception:
            return []

    def _save(self):
        tmp = self.path + ".tmp"
        try:
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump(self.entries, fh, ensure_ascii=False)
            os.replace(tmp, self.path)
        except Exception as exc:
            print("Failed to save history:", exc)

    def _trim(self):
        if len(self.entries) <= self.limit:
            return
        dropped = self.entries[self.limit:]
        self.entries = self.entries[:self.limit]
        for old in dropped:
            if old.get("kind") == "image":
                self._maybe_delete_image(old.get("file"))

    def _maybe_delete_image(self, fname):
        """Delete the image file only if no entry references it anymore."""
        if not fname:
            return
        still_used = any(e.get("file") == fname for e in self.entries)
        if not still_used:
            try:
                os.remove(os.path.join(self.img_dir, fname))
            except OSError:
                pass

    # --- adding

    def add_text(self, text):
        if self.limit <= 0 or not text.strip():
            return
        with self.lock:
            top = self.entries[0] if self.entries else None
            if top and top.get("kind") == "text" and top.get("text") == text:
                return
            self.entries.insert(0, {
                "id": uuid.uuid4().hex[:12],
                "kind": "text",
                "ts": int(time.time()),
                "text": text,
                "preview": " ".join(text.split())[:120],
            })
            self._trim()
            self._save()
        self.on_change()

    def add_image(self, data, mime):
        if self.limit <= 0:
            return
        digest = hashlib.sha1(data).hexdigest()[:16]
        with self.lock:
            top = self.entries[0] if self.entries else None
            if top and top.get("kind") == "image" and top.get("digest") == digest:
                return
            ext = {"image/png": ".png", "image/jpeg": ".jpg",
                   "image/gif": ".gif", "image/webp": ".webp"}.get(mime, ".bin")
            fname = digest + ext
            fpath = os.path.join(self.img_dir, fname)
            if not os.path.exists(fpath):
                try:
                    with open(fpath, "wb") as fh:
                        fh.write(data)
                except OSError as exc:
                    print("Failed to save history image:", exc)
                    return
            self.entries.insert(0, {
                "id": uuid.uuid4().hex[:12],
                "kind": "image",
                "ts": int(time.time()),
                "digest": digest,
                "file": fname,
                "mime": mime,
                "size": len(data),
            })
            self._trim()
            self._save()
        self.on_change()

    # --- reading / operations

    def public_list(self):
        with self.lock:
            out = []
            for e in self.entries:
                item = {"id": e["id"], "kind": e["kind"], "ts": e["ts"]}
                if e["kind"] == "text":
                    item["preview"] = e.get("preview", "")
                    item["text"] = e.get("text", "")
                else:
                    item["mime"] = e.get("mime")
                    item["size"] = e.get("size")
                out.append(item)
            return out

    def get(self, entry_id):
        with self.lock:
            for e in self.entries:
                if e["id"] == entry_id:
                    return dict(e)
        return None

    def image_bytes(self, entry_id):
        e = self.get(entry_id)
        if not e or e["kind"] != "image":
            return None, None
        try:
            with open(os.path.join(self.img_dir, e["file"]), "rb") as fh:
                return fh.read(), e["mime"]
        except OSError:
            return None, None

    def clear(self):
        with self.lock:
            self.entries = []
            self._save()
            try:
                for name in os.listdir(self.img_dir):
                    os.remove(os.path.join(self.img_dir, name))
            except OSError:
                pass
        self.on_change()


# ---------------------------------------------------------------- state

class State:
    """Current clipboard (text/image), files and version counters."""

    def __init__(self, share_dir):
        self.share_dir = share_dir
        self.text = ""
        self.origin = "linux"
        self.image = None        # {"id", "mime", "size", "data"}
        self.files = []
        self.version = 1
        self.history_rev = 0
        self.history = None      # assigned in main
        self.cond = threading.Condition()

    def set_text(self, text, origin):
        with self.cond:
            if text == self.text:
                return False
            self.text = text
            self.origin = origin
            self._bump()
            return True

    def set_image(self, data, mime, origin):
        digest = hashlib.sha1(data).hexdigest()[:16]
        with self.cond:
            if self.image and self.image["id"] == digest:
                return False
            self.image = {"id": digest, "mime": mime,
                          "size": len(data), "data": data}
            self.origin = origin
            self._bump()
            return True

    def clear_image(self):
        with self.cond:
            if self.image is None:
                return False
            self.image = None
            self._bump()
            return True

    def set_files(self, files):
        with self.cond:
            if files == self.files:
                return False
            self.files = files
            self._bump()
            return True

    def note_history(self):
        with self.cond:
            self.history_rev += 1
            self._bump()

    def _bump(self):
        self.version += 1
        self.cond.notify_all()

    def wait_for_change(self, known_version, timeout):
        with self.cond:
            if self.version != known_version:
                return self.snapshot()
            self.cond.wait(timeout)
            return self.snapshot()

    def snapshot(self):
        img = None
        if self.image:
            img = {"id": self.image["id"], "mime": self.image["mime"],
                   "size": self.image["size"]}
        return {
            "version": self.version,
            "history_rev": self.history_rev,
            "text": self.text,
            "origin": self.origin,
            "image": img,
            "files": [{"name": f["name"], "size": f["size"]} for f in self.files],
        }


state = None  # created in main


# ---------------------------------------------------------------- watchers

def watch_clipboard():
    while True:
        try:
            targets = backend.targets()
            image_mime = next((t for t in targets if t in IMAGE_TYPES), None)

            if image_mime and backend.supports_images:
                data = backend.read_image(image_mime)
                if data and state.set_image(data, image_mime, "linux"):
                    state.history.add_image(data, image_mime)
                    print(f"[linux -> web] image {image_mime}, {len(data)} bytes")
            else:
                text = backend.read_text()
                if state.set_text(text, "linux"):
                    state.clear_image()
                    state.history.add_text(text)
                    print(f"[linux -> web] text: {' '.join(text.split())[:60]}")
        except Exception as exc:
            print("Clipboard read error:", exc)
        time.sleep(POLL_INTERVAL)


def watch_share_dir():
    while True:
        try:
            entries = []
            with os.scandir(state.share_dir) as it:
                for e in it:
                    if e.is_file() and not e.name.startswith("."):
                        st = e.stat()
                        entries.append({"name": e.name, "size": st.st_size,
                                        "mtime": int(st.st_mtime)})
            entries.sort(key=lambda f: -f["mtime"])
            state.set_files(entries[:100])
        except Exception as exc:
            print("Folder read error:", exc)
        time.sleep(1.0)


def safe_target(directory, filename):
    name = os.path.basename(filename).strip() or "file"
    name = name.replace("\x00", "")
    base, ext = os.path.splitext(name)
    candidate = os.path.join(directory, name)
    counter = 1
    while os.path.exists(candidate):
        candidate = os.path.join(directory, f"{base} ({counter}){ext}")
        counter += 1
    return candidate


# ---------------------------------------------------------------- page

PAGE = r"""<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>clipbridge</title>
<style>
  /* Dark palette — default and for data-theme="dark" */
  :root, :root[data-theme="dark"] {
    --bg: #12151a;
    --panel: #1a1f27;
    --line: #2c333e;
    --ink: #dfe5ec;
    --dim: #7d8896;
    --live: #6fcf97;
  }
  /* Light palette — on explicit choice */
  :root[data-theme="light"] {
    --bg: #f6f7f9;
    --panel: #ffffff;
    --line: #d8dee6;
    --ink: #1c2027;
    --dim: #6a7480;
    --live: #1f9d57;
  }
  /* Auto: if the user hasn't picked a theme manually, follow the system.
     data-theme="auto" (or its absence) + light system -> light colors. */
  @media (prefers-color-scheme: light) {
    :root:not([data-theme="dark"]):not([data-theme="light"]) {
      --bg: #f6f7f9;
      --panel: #ffffff;
      --line: #d8dee6;
      --ink: #1c2027;
      --dim: #6a7480;
      --live: #1f9d57;
    }
  }
  html { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 22px;
    background: var(--bg); color: var(--ink);
    font: 14px/1.5 ui-monospace, "JetBrains Mono", "Cascadia Mono", Consolas, monospace;
    display: flex; flex-direction: column; gap: 14px; min-height: 100vh;
  }
  body.dragging { outline: 2px dashed var(--live); outline-offset: -10px; }
  header { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
  h1 { font-size: 15px; font-weight: 600; margin: 0; letter-spacing: .04em; }
  #status { color: var(--dim); font-size: 12px; }
  #theme, #lang {
    flex: none;
    background: transparent; color: var(--dim);
    border: 1px solid var(--line); border-radius: 6px;
    padding: 5px 12px; font: inherit; font-size: 12px; cursor: pointer;
  }
  #lang { margin-left: auto; }
  #theme:hover, #lang:hover { border-color: var(--dim); color: var(--ink); }
  #theme:focus-visible, #lang:focus-visible { outline: 2px solid var(--live); outline-offset: 2px; }
  #status.live::before {
    content: ""; display: inline-block; width: 7px; height: 7px;
    border-radius: 50%; background: var(--live);
    margin-right: 7px; vertical-align: middle;
  }
  textarea {
    min-height: 26vh; resize: vertical;
    background: var(--panel); color: var(--ink);
    border: 1px solid var(--line); border-radius: 6px;
    padding: 14px; font: inherit; outline: none;
  }
  textarea:focus { border-color: var(--dim); }
  .row { display: flex; gap: 10px; flex-wrap: wrap; align-items: center; }
  button {
    background: transparent; color: var(--ink);
    border: 1px solid var(--line); border-radius: 6px;
    padding: 8px 14px; font: inherit; cursor: pointer;
  }
  button:hover { border-color: var(--dim); }
  button:focus-visible { outline: 2px solid var(--live); outline-offset: 2px; }
  button.mini { padding: 4px 10px; font-size: 12px; }
  .hint { color: var(--dim); font-size: 12px; }
  section { border-top: 1px solid var(--line); padding-top: 12px; }
  .sec-head { display: flex; align-items: center; gap: 10px; margin-bottom: 10px; }
  h2 {
    font-size: 12px; font-weight: 600; color: var(--dim);
    margin: 0; letter-spacing: .08em; text-transform: uppercase;
  }
  .spacer { flex: 1; }
  #preview { display: none; }
  #preview img {
    max-width: 100%; max-height: 40vh; border: 1px solid var(--line);
    border-radius: 6px; background: var(--panel); display: block;
  }
  ul { list-style: none; margin: 0; padding: 0; }
  .files li {
    display: flex; justify-content: space-between; gap: 12px;
    padding: 7px 0; border-bottom: 1px solid var(--line);
  }
  .files li:last-child { border-bottom: none; }
  a { color: var(--ink); }
  .size { color: var(--dim); font-size: 12px; white-space: nowrap; }
  progress { width: 160px; }
  .hist li {
    display: flex; gap: 12px; align-items: center;
    padding: 8px 0; border-bottom: 1px solid var(--line);
  }
  .hist li:last-child { border-bottom: none; }
  .hist .thumb {
    width: 46px; height: 46px; object-fit: cover; flex: none;
    border: 1px solid var(--line); border-radius: 4px; background: var(--panel);
  }
  .hist .body { flex: 1; min-width: 0; }
  .hist .txt { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .hist .meta { color: var(--dim); font-size: 11px; }
  .hist .acts { display: flex; gap: 6px; flex: none; }
  .empty { color: var(--dim); padding: 8px 0; }
</style>
</head>
<body>
  <header>
    <h1>clipbridge</h1>
    <span id="status">…</span>
    <button id="lang" type="button">язык: рус</button>
    <button id="theme" type="button">тема: авто</button>
  </header>

  <textarea id="box" spellcheck="false" data-i18n-placeholder="placeholder"></textarea>

  <div class="row">
    <button id="copy" data-i18n="copyLocal">Скопировать текст в буфер этого ПК</button>
    <button id="push" data-i18n="push">Отправить на линукс</button>
    <button id="pick" data-i18n="pick">Выбрать файлы…</button>
    <input id="file" type="file" multiple hidden>
    <progress id="bar" hidden max="100" value="0"></progress>
    <span class="hint" data-i18n="dropHint">Файлы можно просто перетащить в окно.</span>
  </div>

  <section id="preview">
    <h2 data-i18n="previewTitle">Картинка из буфера линукса</h2>
    <img id="img" data-i18n-alt="previewAlt" alt="Изображение из буфера обмена линукса">
    <p class="hint" data-i18n="previewHint">Правый клик по картинке &rarr; «Копировать изображение» — и она в буфере этого ПК.</p>
  </section>

  <section>
    <div class="sec-head">
      <h2 data-i18n="historyTitle">История</h2>
      <div class="spacer"></div>
      <button id="clear" class="mini" data-i18n="clear">Очистить</button>
    </div>
    <ul id="history" class="hist"><li class="empty" data-i18n="empty">пусто</li></ul>
  </section>

  <section>
    <h2 data-i18n="filesTitle">Общая папка</h2>
    <ul id="files" class="files"><li class="empty" data-i18n="empty">пусто</li></ul>
  </section>

<script>
const box = document.getElementById('box');
const statusEl = document.getElementById('status');
const preview = document.getElementById('preview');
const img = document.getElementById('img');
const filesEl = document.getElementById('files');
const historyEl = document.getElementById('history');
const bar = document.getElementById('bar');
const fileInput = document.getElementById('file');
const TOKEN = new URLSearchParams(location.search).get('token') || '';
const q = s => encodeURIComponent(s);

// --- UI language: ru / en, remembered; default follows the browser ---
const LANG_KEY = 'clipbridge-lang';
const STR = {
  ru: {
    langBtn: 'язык: рус',
    themeTitle: 'Переключить тему',
    themeAuto: 'тема: авто', themeLight: 'тема: светлая', themeDark: 'тема: тёмная',
    darkWord: 'тёмная', lightWord: 'светлая',
    connecting: 'подключение…', badToken: 'неверный токен', online: 'на связи',
    offline: 'нет связи, переподключаюсь…',
    sent: 'текст отправлен на линукс', sendFail: 'отправить не вышло',
    restored: 'возвращено в буфер линукса', restoreFail: 'не удалось вернуть',
    clearFail: 'не удалось очистить',
    uploaded: 'отправлено: ', uploadFail: 'не удалось отправить ',
    copied: 'скопировано в буфер этого ПК', copyFail: 'скопировать не вышло',
    imageName: 'изображение',
    units: ['Б', 'КБ', 'МБ', 'ГБ'],
    justNow: 'только что', minAgo: ' мин назад', hoursAgo: ' ч назад',
    empty: 'пусто', emptyText: '(пусто)', imageWord: 'картинка',
    histImgAlt: 'картинка из истории',
    toHere: 'сюда', toHereTitle: 'В буфер этого ПК', toLinux: 'на линукс',
    confirmClear: 'Очистить историю? Сохранённые картинки будут удалены.',
    placeholder: 'Вставь текст (Ctrl+V) и нажми «Отправить». Картинку из буфера тоже можно вставить сюда — она уедет на линукс сразу.',
    copyLocal: 'Скопировать текст в буфер этого ПК',
    push: 'Отправить на линукс', pick: 'Выбрать файлы…',
    dropHint: 'Файлы можно просто перетащить в окно.',
    previewTitle: 'Картинка из буфера линукса',
    previewAlt: 'Изображение из буфера обмена линукса',
    previewHint: 'Правый клик по картинке → «Копировать изображение» — и она в буфере этого ПК.',
    historyTitle: 'История', clear: 'Очистить', filesTitle: 'Общая папка'
  },
  en: {
    langBtn: 'language: en',
    themeTitle: 'Toggle theme',
    themeAuto: 'theme: auto', themeLight: 'theme: light', themeDark: 'theme: dark',
    darkWord: 'dark', lightWord: 'light',
    connecting: 'connecting…', badToken: 'bad token', online: 'connected',
    offline: 'no connection, retrying…',
    sent: 'text sent to linux', sendFail: 'failed to send',
    restored: 'restored to the linux clipboard', restoreFail: 'failed to restore',
    clearFail: 'failed to clear',
    uploaded: 'sent: ', uploadFail: 'failed to send ',
    copied: "copied to this PC's clipboard", copyFail: 'copy failed',
    imageName: 'image',
    units: ['B', 'KB', 'MB', 'GB'],
    justNow: 'just now', minAgo: ' min ago', hoursAgo: ' h ago',
    empty: 'empty', emptyText: '(empty)', imageWord: 'image',
    histImgAlt: 'image from history',
    toHere: 'here', toHereTitle: "To this PC's clipboard", toLinux: 'to linux',
    confirmClear: 'Clear the history? Saved images will be deleted.',
    placeholder: 'Paste text (Ctrl+V) and press "Send to linux". An image from the clipboard can be pasted here too — it goes to linux right away.',
    copyLocal: "Copy text to this PC's clipboard",
    push: 'Send to linux', pick: 'Choose files…',
    dropHint: 'You can also just drag files into the window.',
    previewTitle: 'Image from the linux clipboard',
    previewAlt: 'Image from the linux clipboard',
    previewHint: 'Right-click the image → "Copy image" — and it is in this PC\'s clipboard.',
    historyTitle: 'History', clear: 'Clear', filesTitle: 'Shared folder'
  }
};

function readLang() {
  try {
    const l = localStorage.getItem(LANG_KEY);
    if (l === 'ru' || l === 'en') return l;
  } catch (e) {}
  return (navigator.language || 'en').toLowerCase().startsWith('ru') ? 'ru' : 'en';
}

let lang = readLang();
const tr = key => STR[lang][key];
const langBtn = document.getElementById('lang');

function applyLang() {
  document.documentElement.lang = lang;
  langBtn.textContent = tr('langBtn');
  for (const el of document.querySelectorAll('[data-i18n]'))
    el.textContent = tr(el.dataset.i18n);
  for (const el of document.querySelectorAll('[data-i18n-placeholder]'))
    el.placeholder = tr(el.dataset.i18nPlaceholder);
  for (const el of document.querySelectorAll('[data-i18n-alt]'))
    el.alt = tr(el.dataset.i18nAlt);
  applyTheme(readTheme());
  setStatus(lastStatus.key, lastStatus.live, lastStatus.extra);
  if (lastData) renderCurrent(lastData);
  if (historyRev >= 0) loadHistory().catch(() => {});
}

langBtn.addEventListener('click', () => {
  lang = lang === 'ru' ? 'en' : 'ru';
  try { localStorage.setItem(LANG_KEY, lang); } catch (e) {}
  applyLang();
});

// --- theme: auto (system) / light / dark, remembered ---
const THEME_KEY = 'clipbridge-theme';
const THEME_ORDER = ['auto', 'light', 'dark'];
const themeBtn = document.getElementById('theme');
const systemDark = window.matchMedia('(prefers-color-scheme: dark)');

function readTheme() {
  try {
    const t = localStorage.getItem(THEME_KEY);
    return THEME_ORDER.includes(t) ? t : 'auto';
  } catch (e) { return 'auto'; }
}

function applyTheme(mode) {
  // In auto mode CSS is in charge (prefers-color-scheme media query),
  // so we just set data-theme="auto"; otherwise pin the choice.
  document.documentElement.setAttribute('data-theme', mode);
  themeBtn.title = tr('themeTitle');
  themeBtn.textContent = mode === 'auto'
    ? `${tr('themeAuto')} (${systemDark.matches ? tr('darkWord') : tr('lightWord')})`
    : tr(mode === 'light' ? 'themeLight' : 'themeDark');
}

function cycleTheme() {
  const next = THEME_ORDER[(THEME_ORDER.indexOf(readTheme()) + 1) % THEME_ORDER.length];
  try { localStorage.setItem(THEME_KEY, next); } catch (e) {}
  applyTheme(next);
}

themeBtn.addEventListener('click', cycleTheme);
// In auto mode, pick up system theme changes on the fly.
systemDark.addEventListener('change', () => {
  if (readTheme() === 'auto') applyTheme('auto');
});

let version = 0;
let historyRev = -1;
let dirty = false;
let lastData = null;
let lastStatus = { key: 'connecting', live: false, extra: '' };

box.addEventListener('input', () => { dirty = true; });

function setStatus(key, live, extra = '') {
  lastStatus = { key, live, extra };
  statusEl.textContent = tr(key) + extra;
  statusEl.classList.toggle('live', !!live);
}

function humanSize(n) {
  const units = tr('units');
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + ' ' + units[i];
}

function ago(ts) {
  const d = Math.max(0, Math.floor(Date.now() / 1000) - ts);
  if (d < 60) return tr('justNow');
  if (d < 3600) return Math.floor(d / 60) + tr('minAgo');
  if (d < 86400) return Math.floor(d / 3600) + tr('hoursAgo');
  return new Date(ts * 1000).toLocaleString();
}

function renderCurrent(data) {
  if (data.origin === 'linux' && !dirty) box.value = data.text;
  if (data.image) {
    img.src = '/image/' + data.image.id + '?token=' + q(TOKEN);
    preview.style.display = 'block';
  } else {
    preview.style.display = 'none';
    img.removeAttribute('src');
  }

  filesEl.innerHTML = '';
  if (!data.files.length) {
    filesEl.innerHTML = '<li class="empty">' + tr('empty') + '</li>';
  } else {
    for (const f of data.files) {
      const li = document.createElement('li');
      const a = document.createElement('a');
      a.href = '/file/' + q(f.name) + '?token=' + q(TOKEN);
      a.textContent = f.name;
      a.download = f.name;
      const s = document.createElement('span');
      s.className = 'size';
      s.textContent = humanSize(f.size);
      li.append(a, s);
      filesEl.appendChild(li);
    }
  }
}

async function loadHistory() {
  const r = await fetch('/history?token=' + q(TOKEN));
  const items = await r.json();
  historyEl.innerHTML = '';
  if (!items.length) {
    historyEl.innerHTML = '<li class="empty">' + tr('empty') + '</li>';
    return;
  }
  for (const it of items) {
    const li = document.createElement('li');

    if (it.kind === 'image') {
      const t = document.createElement('img');
      t.className = 'thumb';
      t.src = '/history/image/' + it.id + '?token=' + q(TOKEN);
      t.alt = tr('histImgAlt');
      li.appendChild(t);
    }

    const body = document.createElement('div');
    body.className = 'body';
    const line = document.createElement('div');
    line.className = 'txt';
    line.textContent = it.kind === 'text'
      ? (it.preview || tr('emptyText'))
      : tr('imageWord') + ' · ' + humanSize(it.size);
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.textContent = ago(it.ts);
    body.append(line, meta);
    li.appendChild(body);

    const acts = document.createElement('div');
    acts.className = 'acts';

    if (it.kind === 'text') {
      const here = document.createElement('button');
      here.className = 'mini';
      here.textContent = tr('toHere');
      here.title = tr('toHereTitle');
      here.onclick = () => copyTextLocal(it.text);
      acts.appendChild(here);
    }

    const toLinux = document.createElement('button');
    toLinux.className = 'mini';
    toLinux.textContent = tr('toLinux');
    toLinux.onclick = () => restore(it.id);
    acts.appendChild(toLinux);

    li.appendChild(acts);
    historyEl.appendChild(li);
  }
}

async function poll() {
  for (;;) {
    try {
      const r = await fetch('/clip?version=' + version + '&token=' + q(TOKEN));
      if (r.status === 403) { setStatus('badToken', false); return; }
      const data = await r.json();
      if (data.version !== version) {
        version = data.version;
        lastData = data;
        renderCurrent(data);
        if (data.history_rev !== historyRev) {
          historyRev = data.history_rev;
          loadHistory().catch(() => {});
        }
      }
      setStatus('online', true);
    } catch (e) {
      setStatus('offline', false);
      await new Promise(res => setTimeout(res, 2000));
    }
  }
}

async function pushText() {
  try {
    await fetch('/clip', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: box.value, token: TOKEN })
    });
    dirty = false;
    setStatus('sent', true);
  } catch (e) { setStatus('sendFail', false); }
}

async function restore(id) {
  try {
    await fetch('/restore?token=' + q(TOKEN), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, token: TOKEN })
    });
    setStatus('restored', true);
  } catch (e) { setStatus('restoreFail', false); }
}

async function clearHistory() {
  if (!confirm(tr('confirmClear'))) return;
  try {
    await fetch('/history/clear?token=' + q(TOKEN), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: TOKEN })
    });
  } catch (e) { setStatus('clearFail', false); }
}

function upload(file, asClipboardImage) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const name = file.name || 'pasted.png';
    xhr.open('POST', '/upload?token=' + q(TOKEN) + '&name=' + q(name)
      + '&clipboard=' + (asClipboardImage ? '1' : '0'));
    xhr.setRequestHeader('Content-Type', file.type || 'application/octet-stream');
    xhr.upload.onprogress = e => {
      if (e.lengthComputable) { bar.hidden = false; bar.value = (e.loaded / e.total) * 100; }
    };
    xhr.onload = () => {
      bar.hidden = true;
      xhr.status === 200 ? resolve() : reject(new Error(xhr.statusText));
    };
    xhr.onerror = () => { bar.hidden = true; reject(new Error('network')); };
    xhr.send(file);
  });
}

async function sendFiles(files, asClipboardImage) {
  for (const f of files) {
    try {
      await upload(f, asClipboardImage && f.type.startsWith('image/'));
      setStatus('uploaded', true, f.name || tr('imageName'));
    } catch (e) { setStatus('uploadFail', false, f.name); }
  }
}

document.addEventListener('paste', e => {
  const items = [...(e.clipboardData?.items || [])];
  const images = items.filter(i => i.kind === 'file' && i.type.startsWith('image/'));
  if (!images.length) return;
  e.preventDefault();
  sendFiles(images.map(i => i.getAsFile()).filter(Boolean), true);
});

['dragenter', 'dragover'].forEach(ev =>
  document.addEventListener(ev, e => { e.preventDefault(); document.body.classList.add('dragging'); }));
['dragleave', 'drop'].forEach(ev =>
  document.addEventListener(ev, e => {
    e.preventDefault();
    if (ev === 'dragleave' && e.relatedTarget) return;
    document.body.classList.remove('dragging');
  }));
document.addEventListener('drop', e => {
  if (e.dataTransfer?.files?.length) sendFiles([...e.dataTransfer.files], false);
});

function copyTextLocal(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  const ok = document.execCommand('copy');
  ta.remove();
  setStatus(ok ? 'copied' : 'copyFail', ok);
}

document.getElementById('push').addEventListener('click', pushText);
document.getElementById('copy').addEventListener('click', () => copyTextLocal(box.value));
document.getElementById('pick').addEventListener('click', () => fileInput.click());
document.getElementById('clear').addEventListener('click', clearHistory);
fileInput.addEventListener('change', () => { sendFiles([...fileInput.files], false); fileInput.value = ''; });
document.addEventListener('keydown', e => {
  if (e.ctrlKey && e.key === 'Enter') { e.preventDefault(); pushText(); }
});

applyLang();
poll();
</script>
</body>
</html>
"""


# ---------------------------------------------------------------- server

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    token = ""

    def log_message(self, *args):
        pass

    def _params(self, query):
        out = {}
        for chunk in query.split("&"):
            if "=" in chunk:
                k, v = chunk.split("=", 1)
                out[k] = unquote(v)
        return out

    def _read_json(self):
        length = int(self.headers.get("Content-Length", 0))
        try:
            return json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return None

    def _send_bytes(self, code, payload, ctype, extra_headers=()):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        for k, v in extra_headers:
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def _send_json(self, code, obj):
        self._send_bytes(code, json.dumps(obj).encode("utf-8"),
                         "application/json; charset=utf-8")

    def _ok_token(self, supplied):
        return not self.token or supplied == self.token

    # --- GET

    def do_GET(self):
        path, _, query = self.path.partition("?")
        params = self._params(query)

        if path == "/":
            self._send_bytes(200, PAGE.encode("utf-8"), "text/html; charset=utf-8")
            return

        if not self._ok_token(params.get("token", "")):
            self._send_json(403, {"error": "bad token"})
            return

        if path == "/clip":
            try:
                known = int(params.get("version", "0"))
            except ValueError:
                known = 0
            self._send_json(200, state.wait_for_change(known, LONGPOLL_TIMEOUT))
            return

        if path == "/history":
            self._send_json(200, state.history.public_list())
            return

        if path.startswith("/history/image/"):
            data, mime = state.history.image_bytes(path[len("/history/image/"):])
            if data is None:
                self._send_json(404, {"error": "no image"})
                return
            self._send_bytes(200, data, mime or "application/octet-stream")
            return

        if path.startswith("/image/"):
            image = state.image
            if not image or image["id"] != path[len("/image/"):]:
                self._send_json(404, {"error": "no image"})
                return
            self._send_bytes(200, image["data"], image["mime"])
            return

        if path.startswith("/file/"):
            name = os.path.basename(unquote(path[len("/file/"):]))
            full = os.path.join(state.share_dir, name)
            if not name or not os.path.isfile(full):
                self._send_json(404, {"error": "no file"})
                return
            ctype = mimetypes.guess_type(name)[0] or "application/octet-stream"
            with open(full, "rb") as fh:
                data = fh.read()
            disposition = f"attachment; filename*=UTF-8''{quote(name)}"
            self._send_bytes(200, data, ctype, [("Content-Disposition", disposition)])
            return

        self._send_json(404, {"error": "not found"})

    # --- POST

    def do_POST(self):
        path, _, query = self.path.partition("?")
        params = self._params(query)

        if path == "/clip":
            data = self._read_json()
            if data is None:
                self._send_json(400, {"error": "bad json"})
                return
            if not self._ok_token(data.get("token", "")):
                self._send_json(403, {"error": "bad token"})
                return
            text = data.get("text", "")
            if state.set_text(text, "web"):
                state.clear_image()
                backend.write_text(text)
                state.history.add_text(text)
                print(f"[web -> linux] text: {' '.join(text.split())[:60]}")
            self._send_json(200, {"ok": True, "version": state.version})
            return

        if path == "/restore":
            data = self._read_json()
            if data is None or not self._ok_token(data.get("token", "")):
                self._send_json(403, {"error": "bad token"})
                return
            entry = state.history.get(data.get("id", ""))
            if not entry:
                self._send_json(404, {"error": "no entry"})
                return
            if entry["kind"] == "text":
                backend.write_text(entry["text"])
                state.set_text(entry["text"], "web")
                state.clear_image()
            else:
                blob, mime = state.history.image_bytes(entry["id"])
                if blob is None:
                    self._send_json(410, {"error": "image gone"})
                    return
                backend.write_image(blob, mime)
                state.set_image(blob, mime, "web")
            self._send_json(200, {"ok": True})
            return

        if path == "/history/clear":
            data = self._read_json()
            if data is None or not self._ok_token(data.get("token", "")):
                self._send_json(403, {"error": "bad token"})
                return
            state.history.clear()
            self._send_json(200, {"ok": True})
            return

        if path == "/upload":
            if not self._ok_token(params.get("token", "")):
                self._send_json(403, {"error": "bad token"})
                return
            length = int(self.headers.get("Content-Length", 0))
            if length <= 0 or length > MAX_UPLOAD:
                self._send_json(413, {"error": "bad size"})
                return

            name = params.get("name", "file")
            ctype = self.headers.get("Content-Type", "application/octet-stream")
            target = safe_target(state.share_dir, name)

            remaining = length
            chunks = []
            keep_in_memory = ctype in IMAGE_TYPES and params.get("clipboard") == "1"
            with open(target, "wb") as fh:
                while remaining > 0:
                    chunk = self.rfile.read(min(65536, remaining))
                    if not chunk:
                        break
                    fh.write(chunk)
                    if keep_in_memory:
                        chunks.append(chunk)
                    remaining -= len(chunk)

            print(f"[web -> linux] file: {os.path.basename(target)} ({length} bytes)")

            if keep_in_memory:
                data = b"".join(chunks)
                backend.write_image(data, ctype)
                state.set_image(data, ctype, "web")
                state.history.add_image(data, ctype)
                print("    and placed into the linux system clipboard")

            self._send_json(200, {"ok": True, "name": os.path.basename(target)})
            return

        self._send_json(404, {"error": "not found"})


# ---------------------------------------------------------------- main

def local_ips():
    import socket
    ips = set()
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))
        ips.add(s.getsockname()[0])
        s.close()
    except Exception:
        pass
    return sorted(ips) or ["<laptop-ip>"]


def main():
    global state, backend

    ap = argparse.ArgumentParser(
        description="Shared clipboard: text, images, files, history")
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--token", default="", help="simple password in the URL")
    ap.add_argument("--dir", default="~/clipbridge", help="shared folder for files")
    ap.add_argument("--history", type=int, default=200,
                    help="how many history entries to keep (0 — disable)")
    args = ap.parse_args()

    backend = Backend()

    share_dir = os.path.abspath(os.path.expanduser(args.dir))
    os.makedirs(share_dir, exist_ok=True)

    state = State(share_dir)
    state.history = History(share_dir, max(args.history, 0), state.note_history)
    Handler.token = args.token

    threading.Thread(target=watch_clipboard, daemon=True).start()
    threading.Thread(target=watch_share_dir, daemon=True).start()

    suffix = f"?token={quote(args.token)}" if args.token else ""
    print("clipbridge is running.")
    print(f"Shared folder: {share_dir}")
    print(f"History: {len(state.history.entries)} entries (limit {args.history})")
    if not backend.supports_images:
        print("Note: xsel cannot handle images — install xclip for image support.")
    print("Open on another device:")
    for ip in local_ips():
        print(f"    http://{ip}:{args.port}/{suffix}")
    print("Ctrl+C to quit.\n")

    server = ThreadingHTTPServer((args.host, args.port), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nStopped.")


if __name__ == "__main__":
    main()
