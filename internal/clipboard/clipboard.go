// Package clipboard is the system clipboard of the machine running the server.
package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Clipboard is the system clipboard of the machine running the server.
// Formats are MIME types: image/* for images, one of uriTypes for files
// copied in a file manager, anything else is read as text.
type Clipboard interface {
	// Name identifies the backend in startup output.
	Name() string
	// SupportsImages reports whether images can be read and written at all.
	SupportsImages() bool
	// Formats lists what is currently on the clipboard, preferred first.
	Formats() []string
	ReadText() string
	ReadImage(mimeType string) []byte
	// ReadFiles returns the local paths of copied files, given one of uriTypes.
	ReadFiles(format string) []string
	WriteText(text string) error
	WriteImage(data []byte, mimeType string) error
}

var ErrNoImages = errors.New("this clipboard backend cannot handle images")

var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// File managers advertise a copied file under these formats instead of text/plain.
var uriTypes = []string{"text/uri-list", "x-special/gnome-copied-files"}

func IsImageType(t string) bool { return slices.Contains(imageTypes, t) }

func IsURIType(t string) bool { return slices.Contains(uriTypes, t) }

// cmdOutput runs argv and returns its stdout; stderr ends up in the error.
// env entries are added on top of the inherited environment.
func cmdOutput(timeout time.Duration, env []string, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), err
}

// cmdInput feeds data to argv's stdin. Output is deliberately not captured:
// xclip -i and wl-copy fork a child that keeps serving the selection and holds
// the inherited pipes open, which would block us until the timeout.
func cmdInput(timeout time.Duration, env []string, data []byte, argv ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

func hasCommands(names ...string) bool {
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return false
		}
	}
	return true
}

// splitLines returns the non-empty trimmed lines of s.
func splitLines(s string) []string {
	var res []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			res = append(res, line)
		}
	}
	return res
}

// parseURIList turns a text/uri-list (or x-special/gnome-copied-files) payload
// into local paths, dropping anything that is not a local file.
func parseURIList(raw string) []string {
	var paths []string
	for _, line := range splitLines(raw) {
		// x-special/gnome-copied-files starts with a "copy"/"cut" line.
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
