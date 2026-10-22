// clipbridge — shared clipboard (text + images + files + history)
// between a Linux or macOS machine and any device with a browser.
//
// Run on the machine whose clipboard should be shared:
//
//	go run .
//	go build && ./clipbridge -port 9000 -token secret -dir ~/clipbridge -history 300
//
// Open the printed address in a browser on the other device. Nothing to install there.
//
// Features:
//
//	text      host <-> browser, automatic in both directions
//	images    host  -> browser as a preview (right click -> copy image)
//	          browser -> host via Ctrl+V, goes straight into the system clipboard
//	files     shared folder: drag&drop in the browser puts a file on the host,
//	          the whole folder is visible and downloadable from both ends
//	history   everything copied (text/images) is written to disk and survives
//	          restarts; any entry can be restored to the host clipboard or this PC's
//
// Dependencies: standard library only. On Linux one of xclip (X11),
// wl-clipboard (Wayland) or xsel (no images); on macOS the built-in
// pbcopy/pbpaste/osascript.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"clipbridge/internal/clipboard"
	"clipbridge/internal/history"
	"clipbridge/internal/netaddr"
	"clipbridge/internal/server"
	"clipbridge/internal/state"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "clipbridge:", err)
		os.Exit(1)
	}
}

func run() error {
	host := flag.String("host", "0.0.0.0", "address the server listens on")
	port := flag.Int("port", 8765, "port")
	token := flag.String("token", "", "simple password in the URL")
	dir := flag.String("dir", "~/clipbridge", "shared folder for files")
	histLimit := flag.Int("history", 200, "how many history entries to keep (0 — disable)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clip, err := clipboard.New(ctx)
	if err != nil {
		return err
	}

	shareDir, err := filepath.Abs(expandHome(*dir))
	if err == nil {
		err = os.MkdirAll(shareDir, 0o755)
	}
	if err != nil {
		return fmt.Errorf("prepare the shared folder: %w", err)
	}

	st := state.New()
	hist, err := history.Open(shareDir, *histLimit, st.NoteHistory)
	if err != nil {
		return err
	}
	srv := server.New(clip, st, hist, shareDir, *token)

	ln, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Cancelled on Ctrl+C, which releases the pending long polls at once.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	srv.Watch(ctx)
	printBanner(clip, shareDir, hist, *histLimit, *port, *token)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	fmt.Println("\nStopped.")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func printBanner(clip clipboard.Clipboard, shareDir string, hist *history.History, limit, port int, token string) {
	suffix := ""
	if token != "" {
		suffix = "?token=" + url.QueryEscape(token)
	}
	fmt.Println("clipbridge is running.")
	fmt.Printf("Clipboard: %s\n", clip.Name())
	fmt.Printf("Shared folder: %s\n", shareDir)
	fmt.Printf("History: %d entries (limit %d)\n", hist.Len(), max(limit, 0))
	if !clip.SupportsImages() {
		fmt.Printf("Note: %s cannot handle images — install xclip for image support.\n", clip.Name())
	}
	fmt.Println("Open on another device:")
	for _, ip := range netaddr.LocalIPs() {
		fmt.Printf("    http://%s:%d/%s\n", ip, port, suffix)
	}
	fmt.Println("Ctrl+C to quit.")
	fmt.Println()
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
