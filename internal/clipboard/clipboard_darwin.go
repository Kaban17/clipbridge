package clipboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// macClipboard is the macOS pasteboard: text through pbcopy/pbpaste, images
// and Finder file copies through JXA scripts run by osascript.
//
// A one-shot osascript costs ~0.25s, too much to spawn every poll, so a single
// long-running JXA helper watches the pasteboard's change count and reports the
// available types; image and file reads are cached per change count. Polling an
// idle pasteboard therefore spawns nothing but pbpaste.
type macClipboard struct {
	// writing is held for the whole of our own write plus the state refresh after
	// it, so the poller never sees the stale pre-write types (it would re-announce
	// the cached previous image as a fresh copy).
	writing sync.Mutex

	mu       sync.Mutex
	count    int64 // pasteboard change count; 0 = not known yet
	types    []string
	imgKey   string // count + MIME type the cached image was read for
	img      []byte
	filesKey int64
	files    []string
}

// Without a UTF-8 locale (e.g. started from launchd) pbcopy/pbpaste treat text
// as MacRoman and mangle anything non-ASCII.
var pbEnv = []string{"LC_CTYPE=UTF-8"}

// Pasteboard type (UTI) for each image MIME type. public.tiff is not listed:
// it is what most macOS apps put on the pasteboard, and it is served converted to PNG.
var macImageUTIs = map[string]string{
	"image/png":  "public.png",
	"image/jpeg": "public.jpeg",
	"image/gif":  "com.compuserve.gif",
	"image/webp": "org.webmproject.webp",
}

// pbState renders the pasteboard's change count and types as one JSON line.
const jxaStateFunc = `function pbState(pb) {
  var t = pb.types, ts = [];
  for (var i = 0; !t.isNil() && i < t.count; i++) ts.push(t.objectAtIndex(i).js);
  return JSON.stringify({n: Number(pb.changeCount), types: ts});
}
`

// Prints pbState whenever the pasteboard changes. Exits on its own once
// clipbridge is gone (reparented to launchd), even if it was killed outright.
const jxaWatch = `ObjC.import("AppKit");
ObjC.bindFunction("getppid", ["int", []]);
` + jxaStateFunc + `var pb = $.NSPasteboard.generalPasteboard;
var out = $.NSFileHandle.fileHandleWithStandardOutput;
var last = -1;
while ($.getppid() != 1) {
  if (pb.changeCount != last) {
    last = pb.changeCount;
    out.writeData($(pbState(pb) + "\n").dataUsingEncoding($.NSUTF8StringEncoding));
  }
  delay(0.3);
}`

const jxaState = `ObjC.import("AppKit");
` + jxaStateFunc + `pbState($.NSPasteboard.generalPasteboard);`

// argv: UTI, output path. Writes nothing if the pasteboard has no such type.
const jxaReadImage = `ObjC.import("AppKit");
function run(argv) {
  var d = $.NSPasteboard.generalPasteboard.dataForType(argv[0]);
  if (d.isNil()) return;
  if (argv[0] == "public.tiff") {
    var rep = $.NSBitmapImageRep.imageRepWithData(d);
    if (rep.isNil()) return;
    d = rep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $({}));
  }
  d.writeToFileAtomically(argv[1], true);
}`

// argv: input path, UTI. Non-PNG images also get a TIFF copy, since many
// macOS apps only paste PNG or TIFF.
const jxaWriteImage = `ObjC.import("AppKit");
function run(argv) {
  var d = $.NSData.dataWithContentsOfFile(argv[0]);
  if (d.isNil()) throw new Error("cannot read " + argv[0]);
  var pb = $.NSPasteboard.generalPasteboard;
  pb.clearContents;
  pb.setDataForType(d, argv[1]);
  if (argv[1] != "public.png") {
    var img = $.NSImage.alloc.initWithData(d);
    if (!img.isNil()) pb.setDataForType(img.TIFFRepresentation, "public.tiff");
  }
}`

// Prints the local paths of the files copied in Finder, one per line.
const jxaReadFiles = `ObjC.import("AppKit");
function run() {
  var u = $.NSPasteboard.generalPasteboard.readObjectsForClassesOptions($([$.NSURL]), $());
  var o = [];
  for (var i = 0; !u.isNil() && i < u.count; i++) {
    var x = u.objectAtIndex(i);
    if (x.isFileURL) o.push(x.path.js);
  }
  return o.join("\n");
}`

func New(ctx context.Context) (Clipboard, error) {
	if !hasCommands("pbcopy", "pbpaste", "osascript") {
		return nil, errors.New("pbcopy, pbpaste or osascript not found")
	}
	m := &macClipboard{}
	go m.watch(ctx)
	return m, nil
}

func osascript(timeout time.Duration, script string, args ...string) ([]byte, error) {
	return cmdOutput(timeout, nil, append([]string{"osascript", "-l", "JavaScript", "-e", script}, args...)...)
}

// watch runs the JXA helper until ctx is done, restarting it if it dies.
func (m *macClipboard) watch(ctx context.Context) {
	for ctx.Err() == nil {
		if err := m.runWatcher(ctx); err != nil && ctx.Err() == nil {
			fmt.Println("Pasteboard watcher stopped, restarting:", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (m *macClipboard) runWatcher(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", jxaWatch)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		m.update(sc.Bytes())
	}
	return cmd.Wait()
}

// update applies a pbState line. Change counts only grow, so a late report from
// the watcher cannot roll back a newer state seen by refresh.
func (m *macClipboard) update(line []byte) {
	var msg struct {
		N     int64    `json:"n"`
		Types []string `json:"types"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.N > m.count {
		m.count, m.types = msg.N, msg.Types
	}
}

// refresh reads the pasteboard state right away instead of waiting for the watcher.
func (m *macClipboard) refresh() {
	if out, err := osascript(3*time.Second, jxaState); err == nil {
		m.update(out)
	}
}

func (m *macClipboard) state() (int64, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count, m.types
}

func (m *macClipboard) Name() string         { return "macOS pasteboard" }
func (m *macClipboard) SupportsImages() bool { return true }

// Formats maps pasteboard UTIs to MIME types.
func (m *macClipboard) Formats() []string {
	m.writing.Lock() // wait out a write in progress
	m.writing.Unlock()
	_, types := m.state()
	// Finder also puts the file icon on the pasteboard as an image: files win.
	if slices.Contains(types, "public.file-url") {
		return []string{"text/uri-list"}
	}
	var res []string
	for _, uti := range types {
		mimeType := ""
		if uti == "public.tiff" {
			mimeType = "image/png"
		}
		for mt, u := range macImageUTIs {
			if u == uti {
				mimeType = mt
			}
		}
		if mimeType != "" && !slices.Contains(res, mimeType) {
			res = append(res, mimeType)
		}
	}
	return append(res, "text/plain")
}

func (m *macClipboard) ReadText() string {
	out, _ := cmdOutput(3*time.Second, pbEnv, "pbpaste")
	return string(out)
}

func (m *macClipboard) ReadImage(mimeType string) []byte {
	count, types := m.state()
	key := fmt.Sprint(count, " ", mimeType)
	m.mu.Lock()
	if count != 0 && m.imgKey == key {
		defer m.mu.Unlock()
		return m.img
	}
	m.mu.Unlock()

	uti := macImageUTIs[mimeType]
	if mimeType == "image/png" && !slices.Contains(types, uti) {
		uti = "public.tiff"
	}
	if uti == "" {
		return nil
	}
	tmp, err := os.CreateTemp("", "clipbridge-*")
	if err != nil {
		return nil
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if _, err := osascript(6*time.Second, jxaReadImage, uti, tmp.Name()); err != nil {
		return nil
	}
	data, _ := os.ReadFile(tmp.Name())

	m.mu.Lock()
	m.imgKey, m.img = key, data
	m.mu.Unlock()
	return data
}

func (m *macClipboard) ReadFiles(string) []string {
	count, _ := m.state()
	m.mu.Lock()
	if count != 0 && m.filesKey == count {
		defer m.mu.Unlock()
		return m.files
	}
	m.mu.Unlock()

	out, err := osascript(3*time.Second, jxaReadFiles)
	if err != nil {
		return nil
	}
	paths := splitLines(string(out))

	m.mu.Lock()
	m.filesKey, m.files = count, paths
	m.mu.Unlock()
	return paths
}

func (m *macClipboard) WriteText(text string) error {
	m.writing.Lock()
	defer m.writing.Unlock()
	defer m.refresh()
	return cmdInput(3*time.Second, pbEnv, []byte(text), "pbcopy")
}

func (m *macClipboard) WriteImage(data []byte, mimeType string) error {
	uti := macImageUTIs[mimeType]
	if uti == "" {
		return fmt.Errorf("unsupported image type %s", mimeType)
	}
	m.writing.Lock()
	defer m.writing.Unlock()
	defer m.refresh()

	tmp, err := os.CreateTemp("", "clipbridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = osascript(6*time.Second, jxaWriteImage, tmp.Name(), uti)
	return err
}
