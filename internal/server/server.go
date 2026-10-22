// Package server is the HTTP API and the pollers that keep the shared state in
// sync with the host clipboard and the shared folder.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clipbridge/internal/clipboard"
	"clipbridge/internal/history"
	"clipbridge/internal/state"
	"clipbridge/web"
)

const (
	longpollTimeout = 25 * time.Second
	maxUpload       = 200 << 20 // 200 MB
	maxJSONBody     = 1 << 20
)

// Server ties the host clipboard, the shared state and the history together.
type Server struct {
	clip     clipboard.Clipboard
	state    *state.State
	history  *history.History
	shareDir string
	token    string
}

// New returns a server for shareDir. An empty token disables auth.
func New(clip clipboard.Clipboard, st *state.State, h *history.History, shareDir, token string) *Server {
	return &Server{clip: clip, state: st, history: h, shareDir: shareDir, token: token}
}

// Watch keeps the state in sync with the host clipboard and the shared folder
// until ctx is done.
func (s *Server) Watch(ctx context.Context) {
	go s.watchClipboard(ctx)
	go s.watchShareDir(ctx)
}

// Handler maps the HTTP API. Every endpoint except the page itself needs the
// token: GET endpoints and /upload take it as a query parameter, the JSON
// endpoints in the request body.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)

	mux.HandleFunc("GET /clip", s.withToken(s.handleGetClip))
	mux.HandleFunc("GET /history", s.withToken(s.handleGetHistory))
	mux.HandleFunc("GET /history/image/{id}", s.withToken(s.handleHistoryImage))
	mux.HandleFunc("GET /image/{id}", s.withToken(s.handleImage))
	mux.HandleFunc("GET /file/{name}", s.withToken(s.handleFile))

	mux.HandleFunc("POST /clip", s.handlePostClip)
	mux.HandleFunc("POST /grab", s.handleGrab)
	mux.HandleFunc("POST /restore", s.handleRestore)
	mux.HandleFunc("POST /history/clear", s.handleClearHistory)
	mux.HandleFunc("POST /upload", s.withToken(s.handleUpload))

	// Unknown paths and methods get a JSON 404 like every other error.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return mux
}

// --- helpers

func (s *Server) validToken(supplied string) bool {
	return s.token == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(s.token)) == 1
}

func (s *Server) withToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.validToken(r.URL.Query().Get("token")) {
			writeError(w, http.StatusForbidden, "bad token")
			return
		}
		h(w, r)
	}
}

// request is the body of the JSON endpoints; each uses only some fields.
type request struct {
	Token string `json:"token"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Index int    `json:"index"`
}

// decodeRequest parses a JSON body (empty is fine) and checks its token,
// writing the error response itself when it returns false.
func (s *Server) decodeRequest(w http.ResponseWriter, r *http.Request) (request, bool) {
	var req request
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad json")
		return req, false
	}
	if !s.validToken(req.Token) {
		writeError(w, http.StatusForbidden, "bad token")
		return req, false
	}
	return req, true
}

func writeBytes(w http.ResponseWriter, code int, data []byte, ctype string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(code)
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		code, data = http.StatusInternalServerError, []byte(`{"error":"encoding failed"}`)
	}
	writeBytes(w, code, data, "application/json; charset=utf-8")
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeClipText and writeClipImage put content into the host clipboard;
// failures are only logged, the browser side has already been updated.
func (s *Server) writeClipText(text string) {
	if err := s.clip.WriteText(text); err != nil {
		fmt.Println("Failed to write text to clipboard:", err)
	}
}

func (s *Server) writeClipImage(data []byte, mimeType string) {
	if !s.clip.SupportsImages() {
		return
	}
	if err := s.clip.WriteImage(data, mimeType); err != nil {
		fmt.Println("Failed to write image to clipboard:", err)
	}
}

// --- GET

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	writeBytes(w, http.StatusOK, web.Page, "text/html; charset=utf-8")
}

func (s *Server) handleGetClip(w http.ResponseWriter, r *http.Request) {
	known, _ := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	writeJSON(w, http.StatusOK, s.state.WaitForChange(r.Context(), known, longpollTimeout))
}

func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.history.List())
}

func (s *Server) handleHistoryImage(w http.ResponseWriter, r *http.Request) {
	data, mimeType, ok := s.history.Image(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no image")
		return
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	writeBytes(w, http.StatusOK, data, mimeType)
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	img := s.state.Image()
	if img == nil || img.ID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "no image")
		return
	}
	writeBytes(w, http.StatusOK, img.Data, img.Mime)
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	full := filepath.Join(s.shareDir, name)
	if name == "." || name == string(filepath.Separator) || !isRegularFile(full) {
		writeError(w, http.StatusNotFound, "no file")
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
}

// --- POST

func (s *Server) handlePostClip(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeRequest(w, r)
	if !ok {
		return
	}
	if s.state.SetText(req.Text, state.OriginWeb) {
		s.state.ClearImage()
		s.writeClipText(req.Text)
		s.history.AddText(req.Text)
		fmt.Printf("[web -> linux] text: %s\n", collapse(req.Text, 60))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.state.Version()})
}

func (s *Server) handleGrab(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeRequest(w, r)
	if !ok {
		return
	}
	// Only files currently on the clipboard can be grabbed — the browser
	// never gets to name a path of its own.
	entry, found := s.state.ClipFile(req.Index)
	if !found {
		writeError(w, http.StatusNotFound, "no file")
		return
	}
	// Grabbing the same file again just hands out the existing copy.
	name := entry.Saved
	if name == "" || !isRegularFile(filepath.Join(s.shareDir, name)) {
		var err error
		if name, err = copyIntoShare(entry, s.shareDir); err != nil {
			fmt.Println("Failed to copy a clipboard file:", err)
			writeError(w, http.StatusInternalServerError, "cannot copy file")
			return
		}
		s.state.MarkGrabbed(req.Index, entry.Path, name)
		fmt.Printf("[linux -> web] grabbed %s as %s\n", entry.Path, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeRequest(w, r)
	if !ok {
		return
	}
	entry, found := s.history.Get(req.ID)
	if !found {
		writeError(w, http.StatusNotFound, "no entry")
		return
	}
	if entry.Kind == history.KindText {
		s.writeClipText(entry.Text)
		s.state.SetText(entry.Text, state.OriginWeb)
		s.state.ClearImage()
	} else {
		data, mimeType, ok := s.history.Image(entry.ID)
		if !ok {
			writeError(w, http.StatusGone, "image gone")
			return
		}
		s.writeClipImage(data, mimeType)
		s.state.SetImage(data, mimeType, state.OriginWeb)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.decodeRequest(w, r); !ok {
		return
	}
	s.history.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUpload streams the body into the shared folder. With clipboard=1, an
// image is also placed into the host clipboard (a paste in the browser).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	length := r.ContentLength
	if length <= 0 || length > maxUpload {
		writeError(w, http.StatusRequestEntityTooLarge, "bad size")
		return
	}
	query := r.URL.Query()
	name := query.Get("name")
	if name == "" {
		name = "file"
	}
	ctype := r.Header.Get("Content-Type")
	toClipboard := clipboard.IsImageType(ctype) && query.Get("clipboard") == "1"

	var buf bytes.Buffer
	var body io.Reader = io.LimitReader(r.Body, length)
	if toClipboard {
		body = io.TeeReader(body, &buf)
	}
	target := safeTarget(s.shareDir, name)
	if err := writeFileFrom(target, body); err != nil {
		fmt.Println("Upload failed:", err)
		writeError(w, http.StatusInternalServerError, "upload failed")
		return
	}
	fmt.Printf("[web -> linux] file: %s (%d bytes)\n", filepath.Base(target), length)

	if toClipboard {
		data := buf.Bytes()
		s.writeClipImage(data, ctype)
		s.state.SetImage(data, ctype, state.OriginWeb)
		s.history.AddImage(data, ctype)
		fmt.Println("    and placed into the linux system clipboard")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": filepath.Base(target)})
}

// collapse joins all whitespace runs into single spaces and caps the result at n
// runes, for log lines. (history has its own copy for previews.)
func collapse(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
