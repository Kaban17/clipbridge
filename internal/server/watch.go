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
	shareDirPollInterval  = time.Second
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
	every(ctx, shareDirPollInterval, func() {
		files, err := scanShareDir(s.shareDir)
		if err != nil {
			fmt.Println("Folder read error:", err)
			return
		}
		s.state.SetFiles(files)
	})
}

// syncClipboard pulls the host clipboard into State and History.
func (s *Server) syncClipboard() {
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
		if len(data) > 0 && s.state.SetImage(data, imgMime, state.OriginHost) {
			s.state.ClearClipFiles()
			s.history.AddImage(data, imgMime)
			fmt.Printf("[linux -> web] image %s, %d bytes\n", imgMime, len(data))
		}
	case uriMime != "":
		// A file manager copied files: announce them instead of pasting the
		// raw "file:///..." URI into the text box.
		if s.state.SetClipFiles(s.clip.ReadFiles(uriMime)) {
			names := s.state.ClipFileNames()
			if names == "" {
				names = "—"
			}
			fmt.Printf("[linux -> web] files in clipboard: %s\n", names)
		}
	default:
		text := s.clip.ReadText()
		s.state.ClearClipFiles()
		if s.state.SetText(text, state.OriginHost) {
			s.state.ClearImage()
			s.history.AddText(text)
			fmt.Printf("[linux -> web] text: %s\n", collapse(text, 60))
		}
	}
}
