// clipbridge — shared clipboard (text + images + files + history)
// between a Linux laptop and any device with a browser.
//
// Run on the LINUX machine:
//
//	go run .
//	go build && ./clipbridge -port 9000 -token secret -dir ~/clipbridge -history 300
//
// Open the printed address in a browser on the other device. Nothing to install there.
//
// Features:
//
//	text      linux <-> browser, automatic in both directions
//	images    linux  -> browser as a preview (right click -> copy image)
//	          browser -> linux via Ctrl+V, goes straight into the system clipboard
//	files     shared folder: drag&drop in the browser puts a file on the linux side,
//	          the whole folder is visible and downloadable from both ends
//	history   everything copied (text/images) is written to disk and survives
//	          restarts; any entry can be restored to the linux clipboard or this PC's
//
// Dependencies: standard library only + xclip (X11) or wl-clipboard (Wayland).
//
//	sudo apt install xclip        # or: sudo apt install wl-clipboard
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pollInterval    = 500 * time.Millisecond
	longpollTimeout = 25 * time.Second
	maxUpload       = 200 << 20 // 200 MB
)

var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// File managers advertise a copied file under these targets instead of text/plain.
var uriTypes = []string{"text/uri-list", "x-special/gnome-copied-files"}

func isImageType(t string) bool {
	return slices.Contains(imageTypes, t)
}

func isURIType(t string) bool {
	return slices.Contains(uriTypes, t)
}

// collapse joins all whitespace runs into single spaces and caps the result at n runes.
func collapse(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func digest16(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])[:16]
}

func newID() string {
	b := make([]byte, 6)
	crand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- backend

// Backend is a wrapper around xclip / xsel / wl-clipboard.
type Backend struct {
	kind string // "wayland", "xclip" or "xsel"
}

func detectBackend() *Backend {
	has := func(name string) bool {
		_, err := exec.LookPath(name)
		return err == nil
	}
	switch {
	case has("wl-paste") && has("wl-copy"):
		return &Backend{"wayland"}
	case has("xclip"):
		return &Backend{"xclip"}
	case has("xsel"):
		return &Backend{"xsel"} // no image support
	}
	fmt.Fprintln(os.Stderr, "Neither xclip, xsel, nor wl-clipboard found.\n"+
		"Install one of them: sudo apt install xclip")
	os.Exit(1)
	return nil
}

func (b *Backend) supportsImages() bool {
	return b.kind == "wayland" || b.kind == "xclip"
}

// output runs a command and returns its stdout, ignoring errors (like an empty clipboard).
func (b *Backend) output(timeout time.Duration, name string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Run()
	return out.Bytes()
}

// input feeds stdin to a command; stdout is discarded so that tools which fork
// to keep serving the selection (xclip -i, wl-copy) do not block us.
func (b *Backend) input(timeout time.Duration, stdin []byte, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.Run()
}

func (b *Backend) targets() []string {
	var out []byte
	switch b.kind {
	case "wayland":
		out = b.output(3*time.Second, "wl-paste", "--list-types")
	case "xclip":
		out = b.output(3*time.Second, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	default:
		return []string{"text/plain"}
	}
	var res []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			res = append(res, s)
		}
	}
	return res
}

func (b *Backend) readText() string {
	switch b.kind {
	case "wayland":
		return string(b.output(3*time.Second, "wl-paste", "--no-newline"))
	case "xclip":
		return string(b.output(3*time.Second, "xclip", "-selection", "clipboard", "-o"))
	default:
		return string(b.output(3*time.Second, "xsel", "--clipboard", "--output"))
	}
}

// readURIs returns the local paths of the files a file manager put on the clipboard.
func (b *Backend) readURIs(mimeType string) []string {
	switch b.kind {
	case "wayland":
		return parseURIList(string(b.output(3*time.Second, "wl-paste", "--type", mimeType)))
	case "xclip":
		return parseURIList(string(b.output(3*time.Second,
			"xclip", "-selection", "clipboard", "-t", mimeType, "-o")))
	default:
		return nil
	}
}

// parseURIList turns a text/uri-list (or x-special/gnome-copied-files) payload
// into local paths, dropping anything that is not a local file.
func parseURIList(raw string) []string {
	var paths []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		// x-special/gnome-copied-files starts with a "copy"/"cut" line.
		if line == "" || strings.HasPrefix(line, "#") || line == "copy" || line == "cut" {
			continue
		}
		if !strings.HasPrefix(line, "file://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Path == "" || slices.Contains(paths, u.Path) {
			continue
		}
		paths = append(paths, u.Path)
	}
	return paths
}

func (b *Backend) writeText(text string) {
	var err error
	switch b.kind {
	case "wayland":
		err = b.input(3*time.Second, []byte(text), "wl-copy")
	case "xclip":
		err = b.input(3*time.Second, []byte(text), "xclip", "-selection", "clipboard", "-i")
	default:
		err = b.input(3*time.Second, []byte(text), "xsel", "--clipboard", "--input")
	}
	if err != nil {
		fmt.Println("Failed to write text to clipboard:", err)
	}
}

func (b *Backend) readImage(mimeType string) []byte {
	if b.kind == "wayland" {
		return b.output(6*time.Second, "wl-paste", "--type", mimeType)
	}
	return b.output(6*time.Second, "xclip", "-selection", "clipboard", "-t", mimeType, "-o")
}

func (b *Backend) writeImage(data []byte, mimeType string) {
	if !b.supportsImages() {
		return
	}
	var err error
	if b.kind == "wayland" {
		err = b.input(6*time.Second, data, "wl-copy", "--type", mimeType)
	} else {
		err = b.input(6*time.Second, data, "xclip", "-selection", "clipboard", "-t", mimeType, "-i")
	}
	if err != nil {
		fmt.Println("Failed to write image to clipboard:", err)
	}
}

// ---------------------------------------------------------------- history

// Entry is one history record; text lives right in the json,
// images are files in .clipbridge/img/ referenced by name.
type Entry struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	TS      int64  `json:"ts"`
	Text    string `json:"text,omitempty"`
	Preview string `json:"preview,omitempty"`
	Digest  string `json:"digest,omitempty"`
	File    string `json:"file,omitempty"`
	Mime    string `json:"mime,omitempty"`
	Size    int    `json:"size,omitempty"`
}

// History is the log of copied items, persisted to disk.
// Shared-folder files are not logged (they are already in place).
type History struct {
	mu       sync.Mutex
	limit    int
	onChange func()
	imgDir   string
	path     string
	entries  []Entry
}

func newHistory(baseDir string, limit int, onChange func()) *History {
	dir := filepath.Join(baseDir, ".clipbridge")
	h := &History{
		limit:    limit,
		onChange: onChange,
		imgDir:   filepath.Join(dir, "img"),
		path:     filepath.Join(dir, "history.json"),
	}
	if err := os.MkdirAll(h.imgDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to create history dir:", err)
		os.Exit(1)
	}
	h.entries = h.load()
	return h
}

// --- disk

func (h *History) load() []Entry {
	data, err := os.ReadFile(h.path)
	if err != nil {
		return nil
	}
	var out []Entry
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func (h *History) saveLocked() {
	entries := h.entries
	if entries == nil {
		entries = []Entry{}
	}
	data, err := json.Marshal(entries)
	if err == nil {
		tmp := h.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, h.path)
		}
	}
	if err != nil {
		fmt.Println("Failed to save history:", err)
	}
}

func (h *History) trimLocked() {
	if len(h.entries) <= h.limit {
		return
	}
	dropped := h.entries[h.limit:]
	h.entries = h.entries[:h.limit]
	for _, old := range dropped {
		if old.Kind == "image" {
			h.maybeDeleteImageLocked(old.File)
		}
	}
}

// maybeDeleteImageLocked deletes the image file only if no entry references it anymore.
func (h *History) maybeDeleteImageLocked(fname string) {
	if fname == "" {
		return
	}
	for _, e := range h.entries {
		if e.File == fname {
			return
		}
	}
	os.Remove(filepath.Join(h.imgDir, fname))
}

// --- adding

func (h *History) addText(text string) {
	if h.limit <= 0 || strings.TrimSpace(text) == "" {
		return
	}
	h.mu.Lock()
	if len(h.entries) > 0 && h.entries[0].Kind == "text" && h.entries[0].Text == text {
		h.mu.Unlock()
		return
	}
	h.entries = append([]Entry{{
		ID:      newID(),
		Kind:    "text",
		TS:      time.Now().Unix(),
		Text:    text,
		Preview: collapse(text, 120),
	}}, h.entries...)
	h.trimLocked()
	h.saveLocked()
	h.mu.Unlock()
	h.onChange()
}

func (h *History) addImage(data []byte, mimeType string) {
	if h.limit <= 0 {
		return
	}
	digest := digest16(data)
	h.mu.Lock()
	if len(h.entries) > 0 && h.entries[0].Kind == "image" && h.entries[0].Digest == digest {
		h.mu.Unlock()
		return
	}
	ext := map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg",
		"image/gif": ".gif", "image/webp": ".webp",
	}[mimeType]
	if ext == "" {
		ext = ".bin"
	}
	fname := digest + ext
	fpath := filepath.Join(h.imgDir, fname)
	if _, err := os.Stat(fpath); err != nil {
		if err := os.WriteFile(fpath, data, 0o644); err != nil {
			fmt.Println("Failed to save history image:", err)
			h.mu.Unlock()
			return
		}
	}
	h.entries = append([]Entry{{
		ID:     newID(),
		Kind:   "image",
		TS:     time.Now().Unix(),
		Digest: digest,
		File:   fname,
		Mime:   mimeType,
		Size:   len(data),
	}}, h.entries...)
	h.trimLocked()
	h.saveLocked()
	h.mu.Unlock()
	h.onChange()
}

// --- reading / operations

func (h *History) publicList() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, 0, len(h.entries))
	for _, e := range h.entries {
		item := map[string]any{"id": e.ID, "kind": e.Kind, "ts": e.TS}
		if e.Kind == "text" {
			item["preview"] = e.Preview
			item["text"] = e.Text
		} else {
			item["mime"] = e.Mime
			item["size"] = e.Size
		}
		out = append(out, item)
	}
	return out
}

func (h *History) get(entryID string) (Entry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.ID == entryID {
			return e, true
		}
	}
	return Entry{}, false
}

func (h *History) imageBytes(entryID string) ([]byte, string) {
	e, ok := h.get(entryID)
	if !ok || e.Kind != "image" {
		return nil, ""
	}
	data, err := os.ReadFile(filepath.Join(h.imgDir, e.File))
	if err != nil {
		return nil, ""
	}
	return data, e.Mime
}

func (h *History) clear() {
	h.mu.Lock()
	h.entries = nil
	h.saveLocked()
	if names, err := os.ReadDir(h.imgDir); err == nil {
		for _, n := range names {
			os.Remove(filepath.Join(h.imgDir, n.Name()))
		}
	}
	h.mu.Unlock()
	h.onChange()
}

// ---------------------------------------------------------------- state

type imageData struct {
	ID   string
	Mime string
	Size int
	Data []byte
}

type fileInfo struct {
	Name  string
	Size  int64
	Mtime int64
}

type snapImage struct {
	ID   string `json:"id"`
	Mime string `json:"mime"`
	Size int    `json:"size"`
}

type snapFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type snapshot struct {
	Version    int64      `json:"version"`
	HistoryRev int64      `json:"history_rev"`
	Text       string     `json:"text"`
	Origin     string     `json:"origin"`
	Image      *snapImage `json:"image"`
	Files      []snapFile `json:"files"`
	ClipFiles  []snapFile `json:"clip_files"`
}

// clipFile is a file copied on linux; the path stays on the server, the browser
// refers to it by index. Saved is the name it got in the shared folder once
// grabbed, so grabbing the same file twice does not pile up copies.
type clipFile struct {
	Name  string
	Path  string
	Size  int64
	Saved string
}

// State is the current clipboard (text/image), files and version counters.
// Long-polling clients block on waitForChange; every mutation bumps the
// version and replaces the `changed` channel, waking all waiters.
type State struct {
	mu         sync.Mutex
	changed    chan struct{}
	shareDir   string
	text       string
	origin     string
	image      *imageData
	files      []fileInfo
	clipFiles  []clipFile
	version    int64
	historyRev int64
	history    *History // assigned in main
}

func newState(shareDir string) *State {
	return &State{
		changed:  make(chan struct{}),
		shareDir: shareDir,
		origin:   "linux",
		version:  1,
	}
}

func (s *State) bumpLocked() {
	s.version++
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *State) setText(text, origin string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.text {
		return false
	}
	s.text = text
	s.origin = origin
	s.bumpLocked()
	return true
}

func (s *State) setImage(data []byte, mimeType, origin string) bool {
	digest := digest16(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.image != nil && s.image.ID == digest {
		return false
	}
	s.image = &imageData{ID: digest, Mime: mimeType, Size: len(data), Data: data}
	s.origin = origin
	s.bumpLocked()
	return true
}

func (s *State) clearImage() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.image == nil {
		return false
	}
	s.image = nil
	s.bumpLocked()
	return true
}

func (s *State) currentImage() *imageData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.image
}

func (s *State) currentVersion() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func filesEqual(a, b []fileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *State) setFiles(files []fileInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if filesEqual(files, s.files) {
		return false
	}
	s.files = files
	s.bumpLocked()
	return true
}

// setClipFiles announces files copied on linux; the browser fetches them on demand.
func (s *State) setClipFiles(paths []string) bool {
	entries := make([]clipFile, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		// Directories cannot be sent as a single file, so skip them.
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		entries = append(entries, clipFile{Name: filepath.Base(path), Path: path, Size: info.Size()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Compare the clipboard content only: Saved is our own bookkeeping, and
	// including it would make every poll look like a change.
	if sameClipFiles(entries, s.clipFiles) {
		return false
	}
	s.clipFiles = entries
	s.bumpLocked()
	return true
}

func sameClipFiles(a, b []clipFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Path != b[i].Path || a[i].Size != b[i].Size {
			return false
		}
	}
	return true
}

// markGrabbed remembers the shared-folder name a clipboard file was copied under.
func (s *State) markGrabbed(index int, path, saved string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The clipboard may have changed while the copy was running.
	if index >= 0 && index < len(s.clipFiles) && s.clipFiles[index].Path == path {
		s.clipFiles[index].Saved = saved
	}
}

func (s *State) clearClipFiles() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clipFiles) == 0 {
		return false
	}
	s.clipFiles = nil
	s.bumpLocked()
	return true
}

func (s *State) clipFile(index int) (clipFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.clipFiles) {
		return clipFile{}, false
	}
	return s.clipFiles[index], true
}

func (s *State) clipFileNames() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.clipFiles))
	for _, f := range s.clipFiles {
		names = append(names, f.Name)
	}
	return strings.Join(names, ", ")
}

func (s *State) noteHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyRev++
	s.bumpLocked()
}

func (s *State) waitForChange(known int64, timeout time.Duration) snapshot {
	s.mu.Lock()
	if s.version != known {
		defer s.mu.Unlock()
		return s.snapshotLocked()
	}
	ch := s.changed
	s.mu.Unlock()

	select {
	case <-ch:
	case <-time.After(timeout):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *State) snapshotLocked() snapshot {
	var img *snapImage
	if s.image != nil {
		img = &snapImage{ID: s.image.ID, Mime: s.image.Mime, Size: s.image.Size}
	}
	files := make([]snapFile, 0, len(s.files))
	for _, f := range s.files {
		files = append(files, snapFile{Name: f.Name, Size: f.Size})
	}
	clipFiles := make([]snapFile, 0, len(s.clipFiles))
	for _, f := range s.clipFiles {
		clipFiles = append(clipFiles, snapFile{Name: f.Name, Size: f.Size})
	}
	return snapshot{
		Version:    s.version,
		HistoryRev: s.historyRev,
		Text:       s.text,
		Origin:     s.origin,
		Image:      img,
		Files:      files,
		ClipFiles:  clipFiles,
	}
}

// ---------------------------------------------------------------- watchers

func watchClipboard(b *Backend, st *State) {
	for {
		imgMime, uriMime := "", ""
		for _, t := range b.targets() {
			if imgMime == "" && b.supportsImages() && isImageType(t) {
				imgMime = t
			}
			if uriMime == "" && isURIType(t) {
				uriMime = t
			}
		}
		switch {
		case imgMime != "":
			data := b.readImage(imgMime)
			if len(data) > 0 && st.setImage(data, imgMime, "linux") {
				st.clearClipFiles()
				st.history.addImage(data, imgMime)
				fmt.Printf("[linux -> web] image %s, %d bytes\n", imgMime, len(data))
			}
		case uriMime != "":
			// A file manager copied files: announce them instead of pasting the
			// raw "file:///..." URI into the text box.
			if st.setClipFiles(b.readURIs(uriMime)) {
				names := st.clipFileNames()
				if names == "" {
					names = "—"
				}
				fmt.Printf("[linux -> web] files in clipboard: %s\n", names)
			}
		default:
			text := b.readText()
			st.clearClipFiles()
			if st.setText(text, "linux") {
				st.clearImage()
				st.history.addText(text)
				fmt.Printf("[linux -> web] text: %s\n", collapse(text, 60))
			}
		}
		time.Sleep(pollInterval)
	}
}

func watchShareDir(st *State) {
	for {
		entries, err := os.ReadDir(st.shareDir)
		if err != nil {
			fmt.Println("Folder read error:", err)
		} else {
			var files []fileInfo
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				info, err := e.Info()
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				files = append(files, fileInfo{Name: e.Name(), Size: info.Size(), Mtime: info.ModTime().Unix()})
			}
			sort.Slice(files, func(i, j int) bool { return files[i].Mtime > files[j].Mtime })
			if len(files) > 100 {
				files = files[:100]
			}
			st.setFiles(files)
		}
		time.Sleep(time.Second)
	}
}

// safeTarget builds a path for an upload: "<name>_<YYYY-MM-DD_HH-MM-SS><ext>".
func safeTarget(directory, filename string) string {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(filename, "\x00", "")))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "file"
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	stamped := base + "_" + time.Now().Format("2006-01-02_15-04-05")
	candidate := filepath.Join(directory, stamped+ext)
	for counter := 1; ; counter++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = filepath.Join(directory, fmt.Sprintf("%s (%d)%s", stamped, counter, ext))
	}
}

// ---------------------------------------------------------------- server

type obj = map[string]any

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, _ := json.Marshal(v)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(code)
	w.Write(data)
}

func writeBytes(w http.ResponseWriter, code int, data []byte, ctype string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(code)
	w.Write(data)
}

type server struct {
	st    *State
	b     *Backend
	token string
}

func (s *server) okToken(supplied string) bool {
	return s.token == "" || supplied == s.token
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleGET(w, r)
	case http.MethodPost:
		s.handlePOST(w, r)
	default:
		writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
	}
}

// --- GET

func (s *server) handleGET(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	query := r.URL.Query()

	if path == "/" {
		writeBytes(w, http.StatusOK, []byte(page), "text/html; charset=utf-8")
		return
	}

	if !s.okToken(query.Get("token")) {
		writeJSON(w, http.StatusForbidden, obj{"error": "bad token"})
		return
	}

	switch {
	case path == "/clip":
		known, _ := strconv.ParseInt(query.Get("version"), 10, 64)
		writeJSON(w, http.StatusOK, s.st.waitForChange(known, longpollTimeout))

	case path == "/history":
		writeJSON(w, http.StatusOK, s.st.history.publicList())

	case strings.HasPrefix(path, "/history/image/"):
		data, mimeType := s.st.history.imageBytes(strings.TrimPrefix(path, "/history/image/"))
		if data == nil {
			writeJSON(w, http.StatusNotFound, obj{"error": "no image"})
			return
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		writeBytes(w, http.StatusOK, data, mimeType)

	case strings.HasPrefix(path, "/image/"):
		img := s.st.currentImage()
		if img == nil || img.ID != strings.TrimPrefix(path, "/image/") {
			writeJSON(w, http.StatusNotFound, obj{"error": "no image"})
			return
		}
		writeBytes(w, http.StatusOK, img.Data, img.Mime)

	case strings.HasPrefix(path, "/file/"):
		name := filepath.Base(strings.TrimPrefix(path, "/file/"))
		full := filepath.Join(s.st.shareDir, name)
		info, err := os.Stat(full)
		if name == "" || name == "." || err != nil || !info.Mode().IsRegular() {
			writeJSON(w, http.StatusNotFound, obj{"error": "no file"})
			return
		}
		ctype := mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		http.ServeFile(w, r, full)

	default:
		writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
	}
}

// --- POST

type postBody struct {
	Text  string `json:"text"`
	Token string `json:"token"`
	ID    string `json:"id"`
	Index int    `json:"index"`
}

func readJSON(r *http.Request) (*postBody, bool) {
	var body postBody
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &body, true
	}
	if json.Unmarshal(data, &body) != nil {
		return nil, false
	}
	return &body, true
}

func (s *server) handlePOST(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/clip":
		body, ok := readJSON(r)
		if !ok {
			writeJSON(w, http.StatusBadRequest, obj{"error": "bad json"})
			return
		}
		if !s.okToken(body.Token) {
			writeJSON(w, http.StatusForbidden, obj{"error": "bad token"})
			return
		}
		if s.st.setText(body.Text, "web") {
			s.st.clearImage()
			s.b.writeText(body.Text)
			s.st.history.addText(body.Text)
			fmt.Printf("[web -> linux] text: %s\n", collapse(body.Text, 60))
		}
		writeJSON(w, http.StatusOK, obj{"ok": true, "version": s.st.currentVersion()})

	case "/grab":
		body, ok := readJSON(r)
		if !ok || !s.okToken(body.Token) {
			writeJSON(w, http.StatusForbidden, obj{"error": "bad token"})
			return
		}
		// Only files currently on the clipboard can be grabbed — the browser
		// never gets to name a path of its own.
		entry, found := s.st.clipFile(body.Index)
		if !found {
			writeJSON(w, http.StatusNotFound, obj{"error": "no file"})
			return
		}
		// Grabbing the same file again just hands out the existing copy.
		name := entry.Saved
		if name == "" || !isRegularFile(filepath.Join(s.st.shareDir, name)) {
			var err error
			if name, err = copyIntoShare(entry, s.st.shareDir); err != nil {
				fmt.Println("Failed to copy a clipboard file:", err)
				writeJSON(w, http.StatusInternalServerError, obj{"error": "cannot copy file"})
				return
			}
			s.st.markGrabbed(body.Index, entry.Path, name)
			fmt.Printf("[linux -> web] grabbed %s as %s\n", entry.Path, name)
		}
		writeJSON(w, http.StatusOK, obj{"ok": true, "name": name})

	case "/restore":
		body, ok := readJSON(r)
		if !ok || !s.okToken(body.Token) {
			writeJSON(w, http.StatusForbidden, obj{"error": "bad token"})
			return
		}
		entry, found := s.st.history.get(body.ID)
		if !found {
			writeJSON(w, http.StatusNotFound, obj{"error": "no entry"})
			return
		}
		if entry.Kind == "text" {
			s.b.writeText(entry.Text)
			s.st.setText(entry.Text, "web")
			s.st.clearImage()
		} else {
			blob, mimeType := s.st.history.imageBytes(entry.ID)
			if blob == nil {
				writeJSON(w, http.StatusGone, obj{"error": "image gone"})
				return
			}
			s.b.writeImage(blob, mimeType)
			s.st.setImage(blob, mimeType, "web")
		}
		writeJSON(w, http.StatusOK, obj{"ok": true})

	case "/history/clear":
		body, ok := readJSON(r)
		if !ok || !s.okToken(body.Token) {
			writeJSON(w, http.StatusForbidden, obj{"error": "bad token"})
			return
		}
		s.st.history.clear()
		writeJSON(w, http.StatusOK, obj{"ok": true})

	case "/upload":
		s.handleUpload(w, r)

	default:
		writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
	}
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// copyIntoShare copies a clipboard file into the shared folder and returns its new name.
func copyIntoShare(entry clipFile, shareDir string) (string, error) {
	src, err := os.Open(entry.Path)
	if err != nil {
		return "", err
	}
	defer src.Close()

	target := safeTarget(shareDir, entry.Name)
	dst, err := os.Create(target)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(target)
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(target)
		return "", err
	}
	return filepath.Base(target), nil
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if !s.okToken(query.Get("token")) {
		writeJSON(w, http.StatusForbidden, obj{"error": "bad token"})
		return
	}
	length := r.ContentLength
	if length <= 0 || length > maxUpload {
		writeJSON(w, http.StatusRequestEntityTooLarge, obj{"error": "bad size"})
		return
	}

	name := query.Get("name")
	if name == "" {
		name = "file"
	}
	ctype := r.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	target := safeTarget(s.st.shareDir, name)

	f, err := os.Create(target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, obj{"error": "cannot write file"})
		return
	}
	keepInMemory := isImageType(ctype) && query.Get("clipboard") == "1"
	var buf bytes.Buffer
	var dst io.Writer = f
	if keepInMemory {
		dst = io.MultiWriter(f, &buf)
	}
	_, copyErr := io.Copy(dst, io.LimitReader(r.Body, length))
	f.Close()
	if copyErr != nil {
		writeJSON(w, http.StatusInternalServerError, obj{"error": "upload failed"})
		return
	}

	fmt.Printf("[web -> linux] file: %s (%d bytes)\n", filepath.Base(target), length)

	if keepInMemory {
		data := buf.Bytes()
		s.b.writeImage(data, ctype)
		s.st.setImage(data, ctype, "web")
		s.st.history.addImage(data, ctype)
		fmt.Println("    and placed into the linux system clipboard")
	}

	writeJSON(w, http.StatusOK, obj{"ok": true, "name": filepath.Base(target)})
}

// ---------------------------------------------------------------- main

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// localIPs returns the addresses a browser on the local network can reach us on.
func localIPs() []string {
	ips := privateIPv4s()
	if len(ips) == 0 {
		return routedIP()
	}
	// The address the default route would use is the most likely to work, so
	// print it first — but only if it survived the filtering in privateIPv4s.
	if primary := routedIP(); len(primary) == 1 {
		if i := slices.Index(ips, primary[0]); i > 0 {
			ips[0], ips[i] = ips[i], ips[0]
		}
	}
	return ips
}

// privateIPv4s lists the private IPv4 addresses of the usable interfaces.
// Interfaces that are down, loopback or point-to-point are skipped: the last
// group is VPN tunnels, whose addresses mean nothing to a device on the LAN.
func privateIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	const unusable = net.FlagLoopback | net.FlagPointToPoint
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&unusable != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			net4, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := net4.IP.To4(); ip != nil && ip.IsPrivate() {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

// routedIP asks the routing table which address would be used to reach the
// internet. With a full-tunnel VPN up that is the tunnel itself, which no other
// device can connect to, so it is only a fallback and a hint about ordering.
func routedIP() []string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return []string{"<laptop-ip>"}
	}
	defer conn.Close()
	return []string{conn.LocalAddr().(*net.UDPAddr).IP.String()}
}

func main() {
	host := flag.String("host", "0.0.0.0", "address the server listens on")
	port := flag.Int("port", 8765, "port")
	token := flag.String("token", "", "simple password in the URL")
	dir := flag.String("dir", "~/clipbridge", "shared folder for files")
	histLimit := flag.Int("history", 200, "how many history entries to keep (0 — disable)")
	flag.Parse()

	backend := detectBackend()

	shareDir, err := filepath.Abs(expandHome(*dir))
	if err == nil {
		err = os.MkdirAll(shareDir, 0o755)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to prepare the shared folder:", err)
		os.Exit(1)
	}

	st := newState(shareDir)
	limit := max(*histLimit, 0)
	st.history = newHistory(shareDir, limit, st.noteHistory)

	go watchClipboard(backend, st)
	go watchShareDir(st)

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		fmt.Println("\nStopped.")
		os.Exit(0)
	}()

	suffix := ""
	if *token != "" {
		suffix = "?token=" + url.QueryEscape(*token)
	}
	fmt.Println("clipbridge is running.")
	fmt.Printf("Shared folder: %s\n", shareDir)
	fmt.Printf("History: %d entries (limit %d)\n", len(st.history.entries), limit)
	if !backend.supportsImages() {
		fmt.Println("Note: xsel cannot handle images — install xclip for image support.")
	}
	fmt.Println("Open on another device:")
	for _, ip := range localIPs() {
		fmt.Printf("    http://%s:%d/%s\n", ip, *port, suffix)
	}
	fmt.Println("Ctrl+C to quit.")
	fmt.Println()

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	if err := http.ListenAndServe(addr, &server{st: st, b: backend, token: *token}); err != nil {
		fmt.Fprintln(os.Stderr, "Server error:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- page

const page = `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>clipbridge</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'%3E%3Crect width='64' height='64' rx='14' fill='%2312151a'/%3E%3Crect x='16' y='14' width='32' height='40' rx='5' fill='none' stroke='%23dfe5ec' stroke-width='4'/%3E%3Crect x='24' y='9' width='16' height='10' rx='3' fill='%2312151a' stroke='%23dfe5ec' stroke-width='4'/%3E%3Cpath d='M24 29h13m-4-4 4 4-4 4M40 44H27m4-4-4 4 4 4' fill='none' stroke='%236fcf97' stroke-width='4' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E">
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
  #clipfiles { display: none; }
  #clipfiles .name { flex: 1; overflow-wrap: anywhere; }
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

  <section id="clipfiles">
    <h2 data-i18n="clipFilesTitle">Файлы в буфере линукса</h2>
    <ul id="clipfilelist" class="files"></ul>
    <p class="hint" data-i18n="clipFilesHint">«Скачать» сохраняет файл на этом устройстве и оставляет копию в общей папке.</p>
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
const clipFilesBox = document.getElementById('clipfiles');
const clipFilesEl = document.getElementById('clipfilelist');
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
    historyTitle: 'История', clear: 'Очистить', filesTitle: 'Общая папка',
    clipFilesTitle: 'Файлы в буфере линукса',
    clipFilesHint: '«Скачать» сохраняет файл на этом устройстве и оставляет копию в общей папке.',
    grab: 'Скачать', grabbing: 'забираю с линукса…',
    grabbed: 'скачивается: ', grabFail: 'не удалось забрать файл'
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
    historyTitle: 'History', clear: 'Clear', filesTitle: 'Shared folder',
    clipFilesTitle: 'Files in the linux clipboard',
    clipFilesHint: '"Download" saves the file on this device and keeps a copy in the shared folder.',
    grab: 'Download', grabbing: 'fetching from linux…',
    grabbed: 'downloading: ', grabFail: 'failed to grab the file'
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
    ? tr('themeAuto') + ' (' + (systemDark.matches ? tr('darkWord') : tr('lightWord')) + ')'
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

  renderClipFiles(data.clip_files || []);

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

// Files copied on linux: shown with a button, nothing is transferred until it is pressed.
function renderClipFiles(items) {
  clipFilesEl.innerHTML = '';
  clipFilesBox.style.display = items.length ? 'block' : 'none';
  items.forEach((f, i) => {
    const li = document.createElement('li');
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = f.name;
    const size = document.createElement('span');
    size.className = 'size';
    size.textContent = humanSize(f.size);
    const btn = document.createElement('button');
    btn.className = 'mini';
    btn.textContent = tr('grab');
    btn.addEventListener('click', () => grab(i, btn));
    li.append(name, size, btn);
    clipFilesEl.appendChild(li);
  });
}

// Start a download of a file from the shared folder into this device's downloads.
function downloadFile(name) {
  const a = document.createElement('a');
  a.href = '/file/' + q(name) + '?token=' + q(TOKEN);
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
}

// Grab = copy the file into the shared folder on linux, then pull it down here.
// The server hands out the same copy if the file was already grabbed.
async function grab(index, btn) {
  btn.disabled = true;
  setStatus('grabbing', true);
  try {
    const r = await fetch('/grab?token=' + q(TOKEN), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ index, token: TOKEN })
    });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || 'failed');
    downloadFile(d.name);
    setStatus('grabbed', true, d.name);
  } catch (e) {
    setStatus('grabFail', false);
  }
  btn.disabled = false;
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
`
