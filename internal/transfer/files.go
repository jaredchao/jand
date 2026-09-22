package transfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 240 || strings.ContainsAny(name, `/\:`) || strings.TrimRight(name, ". ") != name {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) || strings.ContainsRune(`<>"|?*`, c) {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" {
		return false
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
		return false
	}
	return true
}

// An exclusive private directory separates each receipt from the workspace.
// The untrusted remote filename can never choose a directory or overwrite a file.
type sink struct {
	file      *os.File
	dir, path string
	committed bool
}

func newSink(root, name string) (*sink, error) {
	if !validName(name) {
		return nil, errors.New("unsafe filename")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "jand-")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".partial"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		os.Remove(dir)
		return nil, err
	}
	return &sink{file: f, dir: dir, path: filepath.Join(dir, name)}, nil
}

func (s *sink) abort() {
	s.file.Close()
	if !s.committed {
		os.Remove(filepath.Join(s.dir, ".partial"))
		os.Remove(s.dir)
	}
}

func (s *sink) commit() error {
	if err := s.file.Sync(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	// Link is atomic and fails if the final path already exists, including a
	// symlink. If the filesystem cannot link, fail closed; never fall back to
	// a rename that might overwrite a destination.
	if err := os.Link(filepath.Join(s.dir, ".partial"), s.path); err != nil {
		return fmt.Errorf("cannot commit received file: %w", err)
	}
	s.committed = true
	if err := os.Remove(filepath.Join(s.dir, ".partial")); err != nil {
		return fmt.Errorf("saved file but temporary cleanup failed: %w", err)
	}
	return nil
}
