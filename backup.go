package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Backup: settings, subscriptions, cookies and the "already downloaded"
// archives in one ZIP, so a reinstall or a move to Docker is one upload.
// Downloaded files and the job history are not included.
//
// Restoring keeps this machine's listen address and archive folder (a backup
// from the laptop must not point Docker at /home/…), writes the files
// and restarts İndirgen so everything is read fresh.
// ---------------------------------------------------------------------------

const maxBackup = 200 << 20

type backupManifest struct {
	App     string    `json:"app"`
	Version string    `json:"version"`
	Created time.Time `json:"created"`
}

// backupNames lists what goes into a backup, relative to the data dir.
func (s *Server) backupNames() []string {
	names := []string{"config.json", "subscriptions.json", "archive/yt-dlp.txt", "archive/gallery-dl.sqlite3"}
	for _, site := range s.cookies.sites() {
		names = append(names, "cookies/"+site+".txt")
	}
	return names
}

// backupAllowed: only these names are ever written by a restore.
func backupAllowed(name string) bool {
	switch name {
	case "manifest.json", "config.json", "subscriptions.json", "archive/yt-dlp.txt", "archive/gallery-dl.sqlite3":
		return true
	}
	site, ok := strings.CutPrefix(name, "cookies/")
	if !ok || path.Dir(name) != "cookies" {
		return false
	}
	site, ok = strings.CutSuffix(site, ".txt")
	if !ok {
		return false
	}
	h, err := normCookieHost(site)
	return err == nil && h == site
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Save(); err != nil { // the file on disk is what gets copied
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("indirgen-yedek-"+time.Now().Format("2006-01-02")+".zip"))
	zw := zip.NewWriter(w)
	m, _ := json.MarshalIndent(backupManifest{App: project.Name, Version: version, Created: time.Now()}, "", "  ")
	if f, err := zw.Create("manifest.json"); err == nil {
		f.Write(m)
	}
	for _, name := range s.backupNames() {
		src, err := os.Open(s.cfg.Dir(filepath.FromSlash(name)))
		if err != nil {
			continue // not created yet (no subscriptions, no archive…)
		}
		st, _ := src.Stat()
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if st != nil {
			hdr.Modified = st.ModTime()
		}
		if dst, err := zw.CreateHeader(hdr); err == nil {
			_, _ = io.Copy(dst, src)
		}
		src.Close()
	}
	if err := zw.Close(); err != nil {
		slog.Warn("yedek yazılamadı", "hata", err)
	}
}

type restoreResult struct {
	Files   []string `json:"files"`
	Version string   `json:"version"` // İndirgen version that made the backup
	Restart bool     `json:"restart"`
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBackup+1))
	if err != nil {
		writeErr(w, 400, "yedek okunamadı")
		return
	}
	if len(raw) > maxBackup {
		writeErr(w, 413, "yedek dosyası çok büyük")
		return
	}
	res, err := s.restore(raw)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	res.Restart = s.restart != nil
	writeJSON(w, 200, res)
	slog.Info("yedek geri yüklendi, yeniden başlatılıyor", "dosya", len(res.Files), "yedeğin_sürümü", res.Version)
	s.restartSoon()
}

// restore checks the whole backup first and only then writes it.
func (s *Server) restore(raw []byte) (restoreResult, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return restoreResult{}, errors.New("bu bir İndirgen yedeği (ZIP) değil")
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !backupAllowed(f.Name) {
			return restoreResult{}, fmt.Errorf("yedekte beklenmeyen dosya: %s", f.Name)
		}
		if f.UncompressedSize64 > maxBackup {
			return restoreResult{}, fmt.Errorf("%s çok büyük", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return restoreResult{}, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxBackup+1))
		rc.Close()
		if err != nil || len(b) > maxBackup {
			return restoreResult{}, fmt.Errorf("%s okunamadı", f.Name)
		}
		files[f.Name] = b
	}
	var man backupManifest
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil || man.App != project.Name {
		return restoreResult{}, errors.New("bu bir İndirgen yedeği değil (manifest.json yok)")
	}
	delete(files, "manifest.json")

	if b, ok := files["config.json"]; ok {
		merged, err := s.mergeConfig(b)
		if err != nil {
			return restoreResult{}, err
		}
		files["config.json"] = merged
	}
	if b, ok := files["subscriptions.json"]; ok {
		var list []Subscription
		if err := json.Unmarshal(b, &list); err != nil {
			return restoreResult{}, errors.New("subscriptions.json bozuk")
		}
	}

	// Cookies are replaced as a set: sites missing from the backup go.
	hasCookies := false
	for name := range files {
		hasCookies = hasCookies || strings.HasPrefix(name, "cookies/")
	}
	if hasCookies {
		for _, site := range s.cookies.sites() {
			_ = os.Remove(s.cookies.path(site))
		}
	}
	res := restoreResult{Version: man.Version}
	for name, b := range files {
		dst := s.cfg.Dir(filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return res, err
		}
		if err := writeFileAtomic(dst, b, 0o600); err != nil {
			return res, fmt.Errorf("%s yazılamadı: %w", name, err)
		}
		res.Files = append(res.Files, name)
	}
	slices.Sort(res.Files)
	return res, nil
}

// mergeConfig takes the backup's settings but keeps where this installation
// listens and where its archive is.
func (s *Server) mergeConfig(b []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.New("config.json bozuk")
	}
	var probe Config
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, errors.New("config.json okunamadı: " + err.Error())
	}
	s.cfg.mu.RLock()
	listen, library := cmp.Or(s.cfg.fileListen, s.cfg.Listen), cmp.Or(s.cfg.fileLibrary, s.cfg.Library)
	s.cfg.mu.RUnlock()
	m["listen"], m["library"] = listen, library
	return json.MarshalIndent(m, "", "  ")
}
