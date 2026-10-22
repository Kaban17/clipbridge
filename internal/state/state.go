// Package state holds the current clipboard snapshot that browsers long-poll.
package state

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Where the current clipboard content came from. The page only overwrites its
// text box with content from the host, never with what it sent itself.
const (
	OriginHost = "linux"
	OriginWeb  = "web"
)

type Image struct {
	ID   string // digest16 of Data
	Mime string
	Data []byte
}

type FileInfo struct {
	Name  string
	Size  int64
	Mtime int64
}

// ClipFile is a file copied on the host; the path stays on the server, the
// browser refers to it by index. Saved is the name it got in the shared folder
// once grabbed, so grabbing the same file twice does not pile up copies.
type ClipFile struct {
	Name  string
	Path  string
	Size  int64
	Saved string
}

// Snapshot is the /clip long-poll response.
type Snapshot struct {
	Version    int64          `json:"version"`
	HistoryRev int64          `json:"history_rev"`
	Text       string         `json:"text"`
	Origin     string         `json:"origin"`
	Image      *SnapshotImage `json:"image"`
	Files      []SnapshotFile `json:"files"`
	ClipFiles  []SnapshotFile `json:"clip_files"`
}

type SnapshotImage struct {
	ID   string `json:"id"`
	Mime string `json:"mime"`
	Size int    `json:"size"`
}

type SnapshotFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// State is the current clipboard (text/image), files and version counters.
// Long-polling clients block in WaitForChange; every mutation bumps the
// version and replaces the changed channel, waking all waiters.
//
// Setters report whether anything changed, which is how echoes are
// suppressed: content we just wrote to the clipboard comes back unchanged.
type State struct {
	mu         sync.Mutex
	changed    chan struct{}
	text       string
	origin     string
	image      *Image
	files      []FileInfo
	clipFiles  []ClipFile
	version    int64
	historyRev int64
}

func New() *State {
	return &State{
		changed: make(chan struct{}),
		origin:  OriginHost,
		version: 1,
	}
}

func (s *State) bumpLocked() {
	s.version++
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *State) SetText(text, origin string) bool {
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

func (s *State) SetImage(data []byte, mimeType, origin string) bool {
	digest := digest16(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.image != nil && s.image.ID == digest {
		return false
	}
	s.image = &Image{ID: digest, Mime: mimeType, Data: data}
	s.origin = origin
	s.bumpLocked()
	return true
}

func (s *State) ClearImage() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.image != nil {
		s.image = nil
		s.bumpLocked()
	}
}

func (s *State) Image() *Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.image
}

func (s *State) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *State) SetFiles(files []FileInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(files, s.files) {
		s.files = files
		s.bumpLocked()
	}
}

// SetClipFiles announces files copied on the host; the browser fetches them on demand.
func (s *State) SetClipFiles(paths []string) bool {
	entries := make([]ClipFile, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		// Directories cannot be sent as a single file, so skip them.
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		entries = append(entries, ClipFile{Name: filepath.Base(path), Path: path, Size: info.Size()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Compare the clipboard content only: Saved is our own bookkeeping, and
	// including it would make every poll look like a change.
	same := slices.EqualFunc(entries, s.clipFiles, func(a, b ClipFile) bool {
		return a.Name == b.Name && a.Path == b.Path && a.Size == b.Size
	})
	if same {
		return false
	}
	s.clipFiles = entries
	s.bumpLocked()
	return true
}

// MarkGrabbed remembers the shared-folder name a clipboard file was copied under.
func (s *State) MarkGrabbed(index int, path, saved string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The clipboard may have changed while the copy was running.
	if index >= 0 && index < len(s.clipFiles) && s.clipFiles[index].Path == path {
		s.clipFiles[index].Saved = saved
	}
}

func (s *State) ClearClipFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clipFiles) > 0 {
		s.clipFiles = nil
		s.bumpLocked()
	}
}

func (s *State) ClipFile(index int) (ClipFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.clipFiles) {
		return ClipFile{}, false
	}
	return s.clipFiles[index], true
}

func (s *State) ClipFileNames() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.clipFiles))
	for _, f := range s.clipFiles {
		names = append(names, f.Name)
	}
	return strings.Join(names, ", ")
}

// NoteHistory tells clients to re-fetch the history.
func (s *State) NoteHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyRev++
	s.bumpLocked()
}

// WaitForChange returns as soon as the version differs from known, or after
// timeout, or when ctx is done (client gone, server shutting down).
func (s *State) WaitForChange(ctx context.Context, known int64, timeout time.Duration) Snapshot {
	s.mu.Lock()
	if s.version == known {
		ch := s.changed
		s.mu.Unlock()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
		case <-ctx.Done():
		}
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *State) snapshotLocked() Snapshot {
	var img *SnapshotImage
	if s.image != nil {
		img = &SnapshotImage{ID: s.image.ID, Mime: s.image.Mime, Size: len(s.image.Data)}
	}
	files := make([]SnapshotFile, 0, len(s.files))
	for _, f := range s.files {
		files = append(files, SnapshotFile{Name: f.Name, Size: f.Size})
	}
	clipFiles := make([]SnapshotFile, 0, len(s.clipFiles))
	for _, f := range s.clipFiles {
		clipFiles = append(clipFiles, SnapshotFile{Name: f.Name, Size: f.Size})
	}
	return Snapshot{
		Version:    s.version,
		HistoryRev: s.historyRev,
		Text:       s.text,
		Origin:     s.origin,
		Image:      img,
		Files:      files,
		ClipFiles:  clipFiles,
	}
}

func digest16(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])[:16]
}
