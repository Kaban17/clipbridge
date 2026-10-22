// Package history is the persistent log of everything copied: text in a JSON
// file, images as files named by digest.
package history

import (
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Kinds of history entries.
const (
	KindText  = "text"
	KindImage = "image"
)

var imageExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

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

// Item is an Entry as the browser sees it: no file names or digests.
type Item struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	TS      int64  `json:"ts"`
	Text    string `json:"text,omitempty"`
	Preview string `json:"preview,omitempty"`
	Mime    string `json:"mime,omitempty"`
	Size    int    `json:"size,omitempty"`
}

// History is the log of copied items, persisted to disk; newest first.
// Shared-folder files are not logged (they are already in place).
// A limit of 0 disables it.
type History struct {
	mu       sync.Mutex
	limit    int
	onChange func()
	imgDir   string
	path     string
	entries  []Entry
}

func Open(baseDir string, limit int, onChange func()) (*History, error) {
	dir := filepath.Join(baseDir, ".clipbridge")
	h := &History{
		limit:    max(limit, 0),
		onChange: onChange,
		imgDir:   filepath.Join(dir, "img"),
		path:     filepath.Join(dir, "history.json"),
	}
	if err := os.MkdirAll(h.imgDir, 0o755); err != nil {
		return nil, fmt.Errorf("create history dir: %w", err)
	}
	if err := h.load(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *History) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}

// --- disk

func (h *History) load() error {
	data, err := os.ReadFile(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}
	if err := json.Unmarshal(data, &h.entries); err != nil {
		// A corrupt log is not worth refusing to start over.
		fmt.Println("History file is corrupt, starting empty:", err)
		h.entries = nil
	}
	return nil
}

func (h *History) saveLocked() {
	if err := h.writeLocked(); err != nil {
		fmt.Println("Failed to save history:", err)
	}
}

func (h *History) writeLocked() error {
	entries := h.entries
	if entries == nil {
		entries = []Entry{}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

func (h *History) trimLocked() {
	if len(h.entries) <= h.limit {
		return
	}
	dropped := h.entries[h.limit:]
	h.entries = h.entries[:h.limit:h.limit]
	for _, old := range dropped {
		if old.Kind == KindImage {
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

// pushLocked prepends e, trims and saves.
func (h *History) pushLocked(e Entry) {
	e.ID = newID()
	e.TS = time.Now().Unix()
	h.entries = append([]Entry{e}, h.entries...)
	h.trimLocked()
	h.saveLocked()
}

func (h *History) AddText(text string) {
	if h.limit == 0 || strings.TrimSpace(text) == "" {
		return
	}
	h.mu.Lock()
	if len(h.entries) > 0 && h.entries[0].Kind == KindText && h.entries[0].Text == text {
		h.mu.Unlock()
		return
	}
	h.pushLocked(Entry{Kind: KindText, Text: text, Preview: collapse(text, 120)})
	h.mu.Unlock()
	h.onChange()
}

func (h *History) AddImage(data []byte, mimeType string) {
	if h.limit == 0 {
		return
	}
	digest := digest16(data)
	h.mu.Lock()
	if len(h.entries) > 0 && h.entries[0].Kind == KindImage && h.entries[0].Digest == digest {
		h.mu.Unlock()
		return
	}
	ext, ok := imageExts[mimeType]
	if !ok {
		ext = ".bin"
	}
	fname := digest + ext
	fpath := filepath.Join(h.imgDir, fname)
	if _, err := os.Stat(fpath); err != nil {
		if err := os.WriteFile(fpath, data, 0o644); err != nil {
			h.mu.Unlock()
			fmt.Println("Failed to save history image:", err)
			return
		}
	}
	h.pushLocked(Entry{Kind: KindImage, Digest: digest, File: fname, Mime: mimeType, Size: len(data)})
	h.mu.Unlock()
	h.onChange()
}

// --- reading / operations

func (h *History) List() []Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Item, 0, len(h.entries))
	for _, e := range h.entries {
		item := Item{ID: e.ID, Kind: e.Kind, TS: e.TS}
		if e.Kind == KindText {
			item.Text, item.Preview = e.Text, e.Preview
		} else {
			item.Mime, item.Size = e.Mime, e.Size
		}
		out = append(out, item)
	}
	return out
}

func (h *History) Get(id string) (Entry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Image returns the bytes and MIME type of an image entry.
func (h *History) Image(id string) ([]byte, string, bool) {
	e, ok := h.Get(id)
	if !ok || e.Kind != KindImage {
		return nil, "", false
	}
	data, err := os.ReadFile(filepath.Join(h.imgDir, e.File))
	if err != nil {
		return nil, "", false
	}
	return data, e.Mime, true
}

func (h *History) Clear() {
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

// digest16 names image files; it matches state.Image.ID for the same bytes.
func digest16(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])[:16]
}

func newID() string {
	b := make([]byte, 6)
	crand.Read(b)
	return hex.EncodeToString(b)
}

// collapse joins all whitespace runs into single spaces and caps the result at n runes.
func collapse(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
