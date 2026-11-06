package server

import (
	"context"
	"fmt"
	"time"

	"clipbridge/internal/clipboard"
	"clipbridge/internal/state"
)

const (
	clipboardPollInterval = 500 * time.Millisecond
	// The shared folder is rarely touched by hand; our own uploads and grabs
	// rescan it right away (rescanShareDir), so only manual changes wait.
	shareDirPollInterval = 5 * time.Second
)

// every calls fn right away and then every interval until ctx is done.
func every(ctx context.Context, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) watchClipboard(ctx context.Context) {
	every(ctx, clipboardPollInterval, s.syncClipboard)
}

func (s *Server) watchShareDir(ctx context.Context) {
	every(ctx, shareDirPollInterval, s.rescanShareDir)
}

func (s *Server) rescanShareDir() {
	files, err := scanShareDir(s.shareDir)
	if err != nil {
		fmt.Println("Folder read error:", err)
		return
	}
	s.state.SetFiles(files)
}

// syncClipboard pulls the host clipboard into State and History, skipping the
// reads while the backend reports the same sequence number as last time.
func (s *Server) syncClipboard() {
	seq, ok := s.clip.Seq()
	if ok && seq == s.lastSeq {
		return
	}
	if s.readClipboard() && ok {
		s.lastSeq = seq
	}
}

// readClipboard does one full read. It returns false when the read came back
// empty, which may be transient, so the next poll retries it.
func (s *Server) readClipboard() bool {
	imgMime, uriMime := "", ""
	for _, t := range s.clip.Formats() {
		if imgMime == "" && clipboard.IsImageType(t) {
			imgMime = t
		}
		if uriMime == "" && clipboard.IsURIType(t) {
			uriMime = t
		}
	}
	switch {
	case imgMime != "":
		data := s.clip.ReadImage(imgMime)
		if len(data) == 0 {
			return false
		}
		if s.state.SetImage(data, imgMime, state.OriginHost) {
			s.state.ClearClipFiles()
			s.history.AddImage(data, imgMime)
			fmt.Printf("[linux -> web] image %s, %d bytes\n", imgMime, len(data))
		}
	case uriMime != "":
		// A file manager copied files: announce them instead of pasting the
		// raw "file:///..." URI into the text box.
		paths := s.clip.ReadFiles(uriMime)
		if s.state.SetClipFiles(paths) {
			names := s.state.ClipFileNames()
			if names == "" {
				names = "—"
			}
			fmt.Printf("[linux -> web] files in clipboard: %s\n", names)
		}
		return len(paths) > 0
	default:
		text := s.clip.ReadText()
		// Apps often clear the clipboard right before writing to it; that
		// moment must not wipe the browser's text box.
		if text == "" {
			return false
		}
		s.state.ClearClipFiles()
		if s.state.SetText(text, state.OriginHost) {
			s.state.ClearImage()
			s.history.AddText(text)
			fmt.Printf("[linux -> web] text: %s\n", collapse(text, 60))
		}
	}
	return true
}
