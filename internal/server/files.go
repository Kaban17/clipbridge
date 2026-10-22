package server

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"clipbridge/internal/state"
)

const maxListedFiles = 100

// scanShareDir lists the regular, non-hidden files of dir, newest first.
func scanShareDir(dir string) ([]state.FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []state.FileInfo
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, state.FileInfo{Name: e.Name(), Size: info.Size(), Mtime: info.ModTime().Unix()})
	}
	slices.SortFunc(files, func(a, b state.FileInfo) int { return cmp.Compare(b.Mtime, a.Mtime) })
	if len(files) > maxListedFiles {
		files = files[:maxListedFiles]
	}
	return files, nil
}

// safeTarget builds a path for an upload: "<name>_<YYYY-MM-DD_HH-MM-SS><ext>".
func safeTarget(dir, filename string) string {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(filename, "\x00", "")))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "file"
	}
	ext := filepath.Ext(name)
	stamped := strings.TrimSuffix(name, ext) + "_" + time.Now().Format("2006-01-02_15-04-05")
	candidate := filepath.Join(dir, stamped+ext)
	for counter := 1; ; counter++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stamped, counter, ext))
	}
}

// copyIntoShare copies a clipboard file into the shared folder and returns its new name.
func copyIntoShare(entry state.ClipFile, shareDir string) (string, error) {
	src, err := os.Open(entry.Path)
	if err != nil {
		return "", err
	}
	defer src.Close()

	target := safeTarget(shareDir, entry.Name)
	if err := writeFileFrom(target, src); err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// writeFileFrom creates path with the contents of r, removing it on failure.
func writeFileFrom(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
