// Package publish writes and removes rendered pages under out_dir.
//
// Layout:
//
//	out_dir/p/<slug>          symlink -> ../.v/<version>   (served by nginx)
//	out_dir/.v/<version>/     index.html, a/<sha256>.<ext> (not served directly)
//	out_dir/tmp/              staging, same filesystem
//
// A rename(2) of a symlink replaces it atomically, so a page is never half
// written or briefly missing while it is updated.
package publish

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DirMode  fs.FileMode = 0o750
	FileMode fs.FileMode = 0o640

	pagesDir    = "p"
	versionsDir = ".v"
	tmpDir      = "tmp"
)

type Writer struct {
	root string
	gid  int // -1 = leave group alone
}

func NewWriter(outDir, group string) (*Writer, error) {
	gid := -1
	if group != "" {
		if n, err := strconv.Atoi(group); err == nil {
			gid = n
		} else {
			g, err := user.LookupGroup(group)
			if err != nil {
				return nil, fmt.Errorf("publish: file_group %q: %w", group, err)
			}
			gid, _ = strconv.Atoi(g.Gid)
		}
	}
	w := &Writer{root: outDir, gid: gid}
	for _, d := range []string{"", pagesDir, versionsDir, tmpDir} {
		if err := w.mkdir(filepath.Join(outDir, d)); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *Writer) Root() string { return w.root }

func (w *Writer) mkdir(path string) error {
	if err := os.MkdirAll(path, DirMode); err != nil {
		return err
	}
	return w.fix(path, DirMode)
}

func (w *Writer) fix(path string, mode fs.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if w.gid >= 0 {
		if err := os.Lchown(path, -1, w.gid); err != nil {
			return fmt.Errorf("publish: chgrp %s: %w", path, err)
		}
	}
	return nil
}

// Staging is a directory under tmp/ that becomes a version on Commit.
type Staging struct {
	w   *Writer
	dir string
}

func (w *Writer) Stage() (*Staging, error) {
	dir, err := os.MkdirTemp(filepath.Join(w.root, tmpDir), "stage-")
	if err != nil {
		return nil, err
	}
	if err := w.fix(dir, DirMode); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Staging{w: w, dir: dir}, nil
}

func (s *Staging) Discard() { os.RemoveAll(s.dir) }

func (s *Staging) path(rel string) (string, error) {
	clean := filepath.Clean("/" + rel)[1:]
	if clean == "" || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("publish: invalid path %q", rel)
	}
	p := filepath.Join(s.dir, clean)
	if err := s.w.mkdir(filepath.Dir(p)); err != nil {
		return "", err
	}
	return p, nil
}

func (s *Staging) WriteFile(rel string, data []byte) error {
	p, err := s.path(rel)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, FileMode); err != nil {
		return err
	}
	return s.w.fix(p, FileMode)
}

// Create opens a new file for writing; the caller closes it.
func (s *Staging) Create(rel string) (*os.File, error) {
	p, err := s.path(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, FileMode)
	if err != nil {
		return nil, err
	}
	if err := s.w.fix(p, FileMode); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Staging) Remove(rel string) error {
	p, err := s.path(rel)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Rename moves a staged file to another staged path.
func (s *Staging) Rename(from, to string) error {
	pf, err := s.path(from)
	if err != nil {
		return err
	}
	pt, err := s.path(to)
	if err != nil {
		return err
	}
	return os.Rename(pf, pt)
}

// CopyFromVersion copies rel from a committed version into the staging dir.
// It reports false when the source does not exist.
func (s *Staging) CopyFromVersion(version, rel string) (bool, error) {
	if version == "" {
		return false, nil
	}
	src := filepath.Join(s.w.root, versionsDir, filepath.Base(version), filepath.Clean("/" + rel)[1:])
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := s.Create(rel)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}

// Commit turns the staging dir into a version and points the slug at it.
// It returns the version name.
func (w *Writer) Commit(slug string, s *Staging) (string, error) {
	if !validName(slug) {
		s.Discard()
		return "", fmt.Errorf("publish: invalid slug %q", slug)
	}
	version := slug + "." + randomHex(6)
	vdir := filepath.Join(w.root, versionsDir, version)
	if err := os.Rename(s.dir, vdir); err != nil {
		s.Discard()
		return "", err
	}
	old := w.CurrentVersion(slug)

	tmpLink := filepath.Join(w.root, tmpDir, "link-"+randomHex(8))
	if err := os.Symlink(filepath.Join("..", versionsDir, version), tmpLink); err != nil {
		os.RemoveAll(vdir)
		return "", err
	}
	if w.gid >= 0 {
		_ = os.Lchown(tmpLink, -1, w.gid)
	}
	if err := os.Rename(tmpLink, filepath.Join(w.root, pagesDir, slug)); err != nil {
		os.Remove(tmpLink)
		os.RemoveAll(vdir)
		return "", err
	}
	if old != "" && old != version {
		os.RemoveAll(filepath.Join(w.root, versionsDir, old))
	}
	return version, nil
}

// CurrentVersion returns the version a slug points at, or "".
func (w *Writer) CurrentVersion(slug string) string {
	target, err := os.Readlink(filepath.Join(w.root, pagesDir, slug))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// Exists reports whether a slug is currently served.
func (w *Writer) Exists(slug string) bool {
	_, err := os.Stat(filepath.Join(w.root, pagesDir, slug, "index.html"))
	return err == nil
}

// Remove takes a slug offline and deletes its files.
func (w *Writer) Remove(slug string) error {
	if !validName(slug) {
		return fmt.Errorf("publish: invalid slug %q", slug)
	}
	version := w.CurrentVersion(slug)
	link := filepath.Join(w.root, pagesDir, slug)
	if err := os.RemoveAll(link); err != nil {
		return err
	}
	if version != "" {
		return os.RemoveAll(filepath.Join(w.root, versionsDir, version))
	}
	return nil
}

// Sweep removes pages whose slug is not in keep, versions no page points
// at, and leftovers in tmp/. It returns the removed slugs.
func (w *Writer) Sweep(keep map[string]bool) ([]string, error) {
	var removed []string
	referenced := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(w.root, pagesDir))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if keep[e.Name()] {
			if v := w.CurrentVersion(e.Name()); v != "" {
				referenced[v] = true
			}
			continue
		}
		if err := w.Remove(e.Name()); err == nil {
			removed = append(removed, e.Name())
		}
	}
	versions, err := os.ReadDir(filepath.Join(w.root, versionsDir))
	if err != nil {
		return removed, err
	}
	for _, v := range versions {
		if !referenced[v.Name()] {
			os.RemoveAll(filepath.Join(w.root, versionsDir, v.Name()))
		}
	}
	tmp, _ := os.ReadDir(filepath.Join(w.root, tmpDir))
	for _, t := range tmp {
		os.RemoveAll(filepath.Join(w.root, tmpDir, t.Name()))
	}
	return removed, nil
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
