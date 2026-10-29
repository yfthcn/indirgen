package main

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Archive: browse, preview and delete what is in the library folder.
//
// The folder itself is the source of truth (no database), so files added by
// hand or by older versions show up too. Every access goes through os.Root:
// paths cannot leave the library, not with ".." and not through symlinks.
// Paths in the API are relative and slash-separated ("YouTube/Kanal/a.mp4").
// ---------------------------------------------------------------------------

type LibFile struct {
	Path string    `json:"path"`
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
	Kind string    `json:"kind"` // video | audio | image | other
}

type LibDir struct {
	Path  string    `json:"path"`
	Name  string    `json:"name"`
	Files int       `json:"files"`        // files in the whole subtree
	Size  int64     `json:"size"`         // bytes in the whole subtree
	Mod   time.Time `json:"mod,omitzero"` // newest file inside
	Empty bool      `json:"empty,omitempty"`
}

type LibListing struct {
	Path    string    `json:"path"`
	Flat    bool      `json:"flat"`
	Dirs    []LibDir  `json:"dirs"`
	Files   []LibFile `json:"files"`
	Matches int       `json:"matches"` // files that match, before offset/limit
	Offset  int       `json:"offset"`
	Total   LibDir    `json:"total"` // the listed folder's subtree
	Free    int64     `json:"free_bytes"`
	Exists  bool      `json:"exists"` // the library folder exists at all
}

var (
	errLibNotFound = errors.New("arşivde böyle bir dosya ya da klasör yok")
	errLibRoot     = errors.New("arşiv klasörünün kendisi silinemez")
)

type Library struct {
	dir   string // absolute library path
	mu    sync.Mutex
	files []LibFile
	built time.Time
}

func NewLibrary(dir string) *Library {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return &Library{dir: abs}
}

// Invalidate makes the next listing rescan the folder.
func (l *Library) Invalidate() {
	l.mu.Lock()
	l.built = time.Time{}
	l.mu.Unlock()
}

func (l *Library) open() (*os.Root, error) { return os.OpenRoot(l.dir) }

// cleanRel turns user input into a slash path inside the library ("" = root).
func cleanRel(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errLibNotFound
	}
	p := strings.Trim(path.Clean("/"+strings.ReplaceAll(raw, `\`, "/")), "/")
	if p != "" && !fs.ValidPath(p) {
		return "", errLibNotFound
	}
	return p, nil
}

// skipName hides dotfiles and unfinished downloads.
func skipName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	n := strings.ToLower(name)
	for _, suf := range []string{".part", ".ytdl", ".tmp", ".temp"} {
		if strings.HasSuffix(n, suf) {
			return true
		}
	}
	return strings.Contains(n, ".part-frag") || strings.Contains(n, ".temp.")
}

var videoExts = map[string]bool{"mp4": true, "mkv": true, "webm": true, "mov": true, "m4v": true,
	"avi": true, "flv": true, "ts": true, "3gp": true}

func kindOf(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	switch {
	case videoExts[ext]:
		return "video"
	case audioExts[ext]:
		return "audio"
	case imageExts[ext]:
		return "image"
	}
	return "other"
}

// index returns all library files; the scan is cached for a few seconds.
func (l *Library) index() ([]LibFile, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.built.IsZero() && time.Since(l.built) < 5*time.Second {
		return l.files, true
	}
	root, err := l.open()
	if err != nil {
		l.files, l.built = nil, time.Now()
		return nil, false
	}
	defer root.Close()
	var files []LibFile
	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != "." {
				return fs.SkipDir // unreadable folder: leave it out
			}
			return nil
		}
		if p != "." && skipName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() { // dirs, symlinks, devices
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, LibFile{Path: p, Name: d.Name(), Size: info.Size(), Mod: info.ModTime(), Kind: kindOf(d.Name())})
		return nil
	})
	l.files, l.built = files, time.Now()
	return files, true
}

type LibQuery struct {
	Path   string
	Q      string // search in path and name (whole subtree)
	Sort   string // new | old | size | name
	Flat   bool   // every file of the subtree instead of one folder level
	Offset int
	Limit  int
}

func libCmp(sort string) func(a, b LibFile) int {
	switch sort {
	case "old":
		return func(a, b LibFile) int { return a.Mod.Compare(b.Mod) }
	case "size":
		return func(a, b LibFile) int { return cmp.Compare(b.Size, a.Size) }
	case "name":
		return func(a, b LibFile) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) }
	}
	return func(a, b LibFile) int { return b.Mod.Compare(a.Mod) }
}

func dirCmp(sort string) func(a, b LibDir) int {
	switch sort {
	case "old":
		return func(a, b LibDir) int { return a.Mod.Compare(b.Mod) }
	case "size":
		return func(a, b LibDir) int { return cmp.Compare(b.Size, a.Size) }
	case "name":
		return func(a, b LibDir) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) }
	}
	return func(a, b LibDir) int { return b.Mod.Compare(a.Mod) }
}

// add counts f in the folder's totals.
func (d *LibDir) add(f LibFile) {
	d.Files++
	d.Size += f.Size
	if f.Mod.After(d.Mod) {
		d.Mod = f.Mod
	}
}

// folderLevel splits the files below prefix into the ones directly in the
// folder and its subfolders with their totals. Folders with no visible files
// (empty, or only partial downloads) are listed too, so they can be deleted.
func folderLevel(root *os.Root, dirPath, prefix string, sub []LibFile) (files []LibFile, list []LibDir) {
	dirs := map[string]*LibDir{}
	for _, f := range sub {
		name, _, isDeeper := strings.Cut(f.Path[len(prefix):], "/")
		if !isDeeper {
			files = append(files, f)
			continue
		}
		d := dirs[name]
		if d == nil {
			d = &LibDir{Path: prefix + name, Name: name}
			dirs[name] = d
		}
		d.add(f)
	}
	if entries, err := fs.ReadDir(root.FS(), dirPath); err == nil {
		for _, e := range entries {
			if e.IsDir() && !skipName(e.Name()) && dirs[e.Name()] == nil {
				dirs[e.Name()] = &LibDir{Path: prefix + e.Name(), Name: e.Name(), Empty: true}
			}
		}
	}
	list = []LibDir{}
	for _, d := range dirs {
		list = append(list, *d)
	}
	return files, list
}

func (l *Library) List(q LibQuery) (*LibListing, error) {
	rel, err := cleanRel(q.Path)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 200
	}
	q.Offset = max(q.Offset, 0)
	all, exists := l.index()
	res := &LibListing{Path: rel, Offset: q.Offset, Exists: exists, Free: diskFree(l.dir),
		Dirs: []LibDir{}, Files: []LibFile{}}
	res.Total = LibDir{Path: rel}
	if rel != "" {
		res.Total.Name = path.Base(rel)
	}
	if !exists {
		if rel != "" {
			return nil, errLibNotFound
		}
		return res, nil
	}

	root, err := l.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dirPath := cmp.Or(rel, ".")
	if st, err := root.Stat(dirPath); err != nil || !st.IsDir() {
		return nil, errLibNotFound
	}

	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	var sub []LibFile
	for _, f := range all {
		if strings.HasPrefix(f.Path, prefix) {
			sub = append(sub, f)
			res.Total.add(f)
		}
	}

	var files []LibFile
	if needle := searchKey(strings.TrimSpace(q.Q)); needle != "" || q.Flat {
		res.Flat = true
		for _, f := range sub {
			if needle == "" || strings.Contains(searchKey(f.Path[len(prefix):]), needle) {
				files = append(files, f)
			}
		}
	} else {
		files, res.Dirs = folderLevel(root, dirPath, prefix, sub)
		slices.SortFunc(res.Dirs, dirCmp(q.Sort))
	}

	slices.SortFunc(files, libCmp(q.Sort))
	res.Matches = len(files)
	if q.Offset < len(files) {
		res.Files = files[q.Offset:min(len(files), q.Offset+q.Limit)]
	}
	return res, nil
}

// OpenFile opens a library file for reading. The caller closes it.
func (l *Library) OpenFile(raw string) (*os.File, fs.FileInfo, error) {
	rel, err := cleanRel(raw)
	if err != nil || rel == "" {
		return nil, nil, errLibNotFound
	}
	root, err := l.open()
	if err != nil {
		return nil, nil, errLibNotFound
	}
	defer root.Close() // the opened file stays valid
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, errLibNotFound
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, errLibNotFound
	}
	return f, st, nil
}

// ZipName is the download name for a folder: its own name, or the
// library's name for the whole archive.
func (l *Library) ZipName(raw string) string {
	rel, _ := cleanRel(raw)
	name := filepath.Base(l.dir)
	if rel != "" {
		name = path.Base(rel)
	}
	return cmp.Or(sanitizeFilename(name), "indirgen") + ".zip"
}

// Walk calls fn for every visible file below a folder, for ZIP downloads.
func (l *Library) Walk(raw string, fn func(rel string, f *os.File, st fs.FileInfo) error) (int, error) {
	rel, err := cleanRel(raw)
	if err != nil {
		return 0, err
	}
	root, err := l.open()
	if err != nil {
		return 0, errLibNotFound
	}
	defer root.Close()
	dirPath := cmp.Or(rel, ".")
	if st, err := root.Stat(dirPath); err != nil || !st.IsDir() {
		return 0, errLibNotFound
	}
	n := 0
	err = fs.WalkDir(root.FS(), dirPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != dirPath && skipName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return nil
		}
		n++
		return fn(strings.TrimPrefix(strings.TrimPrefix(p, dirPath), "/"), f, st)
	})
	return n, err
}

type LibDeleted struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Delete removes a file, or a folder with everything in it, then any parent
// folders left empty. It returns the absolute path that was removed.
func (l *Library) Delete(raw string) (LibDeleted, string, error) {
	var res LibDeleted
	rel, err := cleanRel(raw)
	if err != nil {
		return res, "", err
	}
	if rel == "" {
		return res, "", errLibRoot
	}
	root, err := l.open()
	if err != nil {
		return res, "", errLibNotFound
	}
	defer root.Close()
	defer l.Invalidate()

	st, err := root.Lstat(rel)
	if err != nil {
		return res, "", errLibNotFound
	}
	if !st.IsDir() {
		if err := root.Remove(rel); err != nil {
			return res, "", err
		}
		res.Files, res.Bytes = 1, st.Size()
	} else {
		// Files first, then folders deepest first. Hidden and partial files
		// go too: the folder is meant to disappear.
		var dirs []string
		var firstErr error
		_ = fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				firstErr = cmp.Or(firstErr, err)
				return nil
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			info, ierr := d.Info()
			if err := root.Remove(p); err != nil {
				firstErr = cmp.Or(firstErr, err)
				return nil
			}
			res.Files++
			if ierr == nil && d.Type().IsRegular() {
				res.Bytes += info.Size()
			}
			return nil
		})
		for _, d := range slices.Backward(dirs) {
			if err := root.Remove(d); err != nil {
				firstErr = cmp.Or(firstErr, err)
			}
		}
		if firstErr != nil {
			return res, "", firstErr
		}
	}
	for p := path.Dir(rel); p != "." && p != "/"; p = path.Dir(p) {
		if root.Remove(p) != nil { // not empty: stop
			break
		}
	}
	return res, filepath.Join(l.dir, filepath.FromSlash(rel)), nil
}
