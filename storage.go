package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// Where downloads go: the archive folder and its layout, chosen in Settings.
// A new folder takes effect after a restart (the archive view, file serving
// and deletion are all confined to it); files already downloaded stay where
// they are. The folder cannot be changed here when it comes from -library
// (Docker sets it to the /downloads volume).
// ---------------------------------------------------------------------------

type StorageInfo struct {
	Library string `json:"library"`           // folder in use
	Pending string `json:"pending,omitempty"` // saved, used after the restart
	Layout  string `json:"layout"`            // site | flat
	CanEdit bool   `json:"can_edit"`
	Reason  string `json:"reason,omitempty"` // docker | flag
	Free    int64  `json:"free"`
	Home    string `json:"home"`
}

func (c *Config) Storage() StorageInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := StorageInfo{Library: c.Library, Layout: c.Layout, CanEdit: true}
	if c.fileLibrary != "" && c.fileLibrary != c.Library && !c.libraryFlag {
		st.Pending = c.fileLibrary
	}
	switch {
	case inDocker():
		st.CanEdit, st.Reason = false, "docker"
	case c.libraryFlag:
		st.CanEdit, st.Reason = false, "flag"
	}
	return st
}

func (c *Config) SetLayout(layout string) error {
	if layout != "site" && layout != "flat" {
		return errors.New("geçersiz klasör düzeni")
	}
	c.mu.Lock()
	c.Layout = layout
	c.mu.Unlock()
	return c.Save()
}

// checkLibraryDir makes sure dir can be the archive: absolute, not the data
// folder (or inside it) and writable. It is created when missing.
func (c *Config) checkLibraryDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if rest, ok := strings.CutPrefix(dir, "~"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		home, _ := os.UserHomeDir()
		dir = home + rest
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("tam yol gerekli, örneğin /home/kullanici/Videolar")
	}
	dir = filepath.Clean(dir)
	data, _ := filepath.Abs(c.dataDir)
	if dir == "/" || isWithin(dir, data) || isWithin(data, dir) {
		return "", errors.New("bu klasör arşiv olamaz (kök ya da İndirgen'in veri klasörü)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("klasör oluşturulamadı: %w", err)
	}
	f, err := os.CreateTemp(dir, ".indirgen-wtest-*")
	if err != nil {
		return "", fmt.Errorf("bu klasöre yazılamıyor: %w", err)
	}
	f.Close()
	os.Remove(f.Name())
	return dir, nil
}

// SetLibraryDir stores a new archive folder; it is used from the next start.
func (c *Config) SetLibraryDir(dir string) (string, error) {
	if st := c.Storage(); !st.CanEdit {
		return "", errors.New("arşiv klasörü burada değiştirilemez (" + st.Reason + ")")
	}
	dir, err := c.checkLibraryDir(dir)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.fileLibrary = dir
	c.mu.Unlock()
	return dir, c.Save()
}

// ---- folder picker ----------------------------------------------------------

type DirListing struct {
	Path     string   `json:"path"`
	Parent   string   `json:"parent,omitempty"`
	Dirs     []string `json:"dirs"`
	Writable bool     `json:"writable"`
	Places   []Place  `json:"places"`
}

type Place struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// places: home, the usual media folders and mounted disks.
func places() []Place {
	home, _ := os.UserHomeDir()
	var out []Place
	add := func(name, p string) {
		if st, err := os.Stat(p); err == nil && st.IsDir() && !slices.ContainsFunc(out, func(x Place) bool { return x.Path == p }) {
			out = append(out, Place{name, p})
		}
	}
	add("~", home)
	for _, n := range []string{"Downloads", "İndirilenler", "Videos", "Videolar", "Music", "Müzik", "Pictures", "Resimler"} {
		add(n, filepath.Join(home, n))
	}
	user := filepath.Base(home)
	for _, root := range []string{"/media/" + user, "/run/media/" + user, "/mnt", "/srv"} {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				add(e.Name(), filepath.Join(root, e.Name()))
			}
		}
	}
	return out
}

func listDirs(dir string) (DirListing, error) {
	if !filepath.IsAbs(dir) {
		return DirListing{}, errors.New("tam yol gerekli")
	}
	dir = filepath.Clean(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return DirListing{}, fmt.Errorf("klasör açılamadı: %w", err)
	}
	l := DirListing{Path: dir, Dirs: []string{}}
	if p := filepath.Dir(dir); p != dir {
		l.Parent = p
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() || (e.Type()&os.ModeSymlink != 0 && isDir(filepath.Join(dir, e.Name()))) {
			l.Dirs = append(l.Dirs, e.Name())
		}
	}
	slices.SortFunc(l.Dirs, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	if f, err := os.CreateTemp(dir, ".indirgen-wtest-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		l.Writable = true
	}
	return l, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---- HTTP -------------------------------------------------------------------

func (s *Server) storageInfo() StorageInfo {
	st := s.cfg.Storage()
	st.Free = diskFree(st.Library)
	st.Home, _ = os.UserHomeDir()
	return st
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Library *string `json:"library"`
		Layout  *string `json:"layout"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if body.Layout != nil {
		if err := s.cfg.SetLayout(*body.Layout); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	restart := false
	if body.Library != nil {
		dir, err := s.cfg.SetLibraryDir(*body.Library)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		restart = dir != s.cfg.Storage().Library
		if !restart { // back to the folder in use: nothing pending
			s.cfg.mu.Lock()
			s.cfg.fileLibrary = s.cfg.Library
			s.cfg.mu.Unlock()
		}
	}
	st := s.storageInfo()
	writeJSON(w, 200, map[string]any{"storage": st, "restart": restart && s.restart != nil})
	if restart {
		s.restartSoon()
	}
}

func (s *Server) handleDirs(w http.ResponseWriter, r *http.Request) {
	p := cmpOr(r.URL.Query().Get("path"), s.cfg.Storage().Library)
	// A folder that does not exist (yet): show the nearest one that does.
	for i := 0; i < 20 && filepath.IsAbs(p) && !isDir(p); i++ {
		p = parentDir(p)
	}
	if !isDir(p) {
		p, _ = os.UserHomeDir()
	}
	l, err := listDirs(p)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	l.Places = places()
	writeJSON(w, 200, l)
}

// POST /api/dirs {path, name}: a new folder inside path.
func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct{ Path, Name string }
	if !readBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || len(name) > 120 {
		writeErr(w, 400, "geçersiz klasör adı")
		return
	}
	if !filepath.IsAbs(body.Path) || !isDir(body.Path) {
		writeErr(w, 400, "üst klasör bulunamadı")
		return
	}
	p := filepath.Join(filepath.Clean(body.Path), name)
	if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		writeErr(w, 422, "klasör oluşturulamadı: "+err.Error())
		return
	}
	l, err := listDirs(p)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	l.Places = places()
	writeJSON(w, 200, l)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
