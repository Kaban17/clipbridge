//go:build !darwin

package clipboard

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// toolClipboard drives a command-line clipboard tool. Each field is the argv
// for one operation; readAs/writeAs are nil for tools without image support.
type toolClipboard struct {
	name      string
	needs     []string // binaries that must be installed
	list      []string // nil: the tool cannot list formats, assume text
	readText  []string
	writeText []string
	readAs    func(format string) []string
	writeAs   func(format string) []string
}

var clipboardTools = []*toolClipboard{
	{
		name:      "wl-clipboard",
		needs:     []string{"wl-paste", "wl-copy"},
		list:      []string{"wl-paste", "--list-types"},
		readText:  []string{"wl-paste", "--no-newline"},
		writeText: []string{"wl-copy"},
		readAs:    func(f string) []string { return []string{"wl-paste", "--type", f} },
		writeAs:   func(f string) []string { return []string{"wl-copy", "--type", f} },
	},
	{
		name:      "xclip",
		needs:     []string{"xclip"},
		list:      []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"},
		readText:  []string{"xclip", "-selection", "clipboard", "-o"},
		writeText: []string{"xclip", "-selection", "clipboard", "-i"},
		readAs:    func(f string) []string { return []string{"xclip", "-selection", "clipboard", "-t", f, "-o"} },
		writeAs:   func(f string) []string { return []string{"xclip", "-selection", "clipboard", "-t", f, "-i"} },
	},
	{
		name:      "xsel",
		needs:     []string{"xsel"},
		readText:  []string{"xsel", "--clipboard", "--output"},
		writeText: []string{"xsel", "--clipboard", "--input"},
	},
}

// New picks the first installed tool, in order of preference.
func New(context.Context) (Clipboard, error) {
	for _, t := range clipboardTools {
		if hasCommands(t.needs...) {
			return t, nil
		}
	}
	return nil, errors.New("neither xclip, xsel, nor wl-clipboard found\n" +
		"Install one of them: sudo apt install xclip")
}

func (t *toolClipboard) Name() string         { return t.name }
func (t *toolClipboard) SupportsImages() bool { return t.readAs != nil }

func (t *toolClipboard) Formats() []string {
	if t.list == nil {
		return []string{"text/plain"}
	}
	out, _ := cmdOutput(3*time.Second, nil, t.list...)
	return splitLines(string(out))
}

// Read errors are ignored: an empty clipboard is reported as one.

func (t *toolClipboard) ReadText() string {
	out, _ := cmdOutput(3*time.Second, nil, t.readText...)
	return string(out)
}

func (t *toolClipboard) ReadImage(mimeType string) []byte {
	if t.readAs == nil {
		return nil
	}
	out, _ := cmdOutput(6*time.Second, nil, t.readAs(mimeType)...)
	return out
}

func (t *toolClipboard) ReadFiles(format string) []string {
	if t.readAs == nil {
		return nil
	}
	out, _ := cmdOutput(3*time.Second, nil, t.readAs(format)...)
	return parseURIList(string(out))
}

func (t *toolClipboard) WriteText(text string) error {
	return cmdInput(3*time.Second, nil, []byte(text), t.writeText...)
}

func (t *toolClipboard) WriteImage(data []byte, mimeType string) error {
	if t.writeAs == nil {
		return fmt.Errorf("%s: %w", t.name, ErrNoImages)
	}
	return cmdInput(6*time.Second, nil, data, t.writeAs(mimeType)...)
}
