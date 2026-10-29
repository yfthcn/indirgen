package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitAndDesktopFiles(t *testing.T) {
	u := unitFile("/home/a b/.local/bin/indirgen", "/home/a b/.local/share/indirgen")
	if !strings.Contains(u, `ExecStart="/home/a b/.local/bin/indirgen" serve -data "/home/a b/.local/share/indirgen"`) ||
		!strings.Contains(u, "WantedBy=default.target") {
		t.Fatal(u)
	}
	// systemd's own parser, when available.
	if _, err := exec.LookPath("systemd-analyze"); err == nil {
		dir := t.TempDir()
		bin := filepath.Join(dir, "indirgen")
		os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
		unit := filepath.Join(dir, serviceName)
		os.WriteFile(unit, []byte(unitFile(bin, dir)), 0o644)
		out, err := exec.Command("systemd-analyze", "verify", "--man=no", unit).CombinedOutput()
		if err != nil && strings.Contains(string(out), serviceName+":") {
			t.Fatalf("systemd-analyze: %s", out)
		}
	}
	d := desktopFile("http://127.0.0.1:8899", "/x/indirgen.svg")
	if !strings.Contains(d, "Exec=xdg-open http://127.0.0.1:8899") || !strings.HasPrefix(d, "[Desktop Entry]") {
		t.Fatal(d)
	}
	if localURL("0.0.0.0:9000") != "http://127.0.0.1:9000" || localURL("") != "http://127.0.0.1:8899" {
		t.Fatal("localURL")
	}
}

func TestUserPathsFollowXDG(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "/data/share")
	p, _ := userPaths()
	if p.Bin != "/home/u/.local/bin/indirgen" || p.Unit != "/home/u/.config/systemd/user/indirgen.service" ||
		p.Desktop != "/data/share/applications/indirgen.desktop" {
		t.Fatalf("%+v", p)
	}
}

func TestUnitDataDirRoundTrip(t *testing.T) {
	for _, dir := range []string{"/home/u/.local/share/indirgen", "/home/a b/veri", `/home/u/100%/$HOME/"x"`} {
		got := unitDataDir(writeUnit(t, unitFile("/home/u/.local/bin/indirgen", dir)))
		if got != dir {
			t.Errorf("%q → %q", dir, got)
		}
	}
	if unitDataDir("/nonexistent") != "" {
		t.Fatal("missing unit")
	}
}

func writeUnit(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), serviceName)
	os.WriteFile(p, []byte(content), 0o644)
	return p
}

func TestLocalURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8899": "http://127.0.0.1:8899", "0.0.0.0:9000": "http://127.0.0.1:9000", "[::]:8899": "http://127.0.0.1:8899",
		"10.0.0.1:8899": "http://10.0.0.1:8899", "localhost:7000": "http://127.0.0.1:7000", "": "http://127.0.0.1:8899",
	} {
		if got := localURL(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestPeekConfigWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "yok")
	c := PeekConfig(dir)
	if c.Listen != "127.0.0.1:8899" {
		t.Fatal(c.Listen)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("PeekConfig created the data folder")
	}
}

// uninstall -purge deletes only a folder that is İndirgen's data folder.
func TestPurgeAllowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(k, "")
	}
	xdgData := filepath.Join(t.TempDir(), "share")
	mk := func(dir string, files ...string) string {
		os.MkdirAll(dir, 0o755)
		for _, f := range files {
			os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o600)
		}
		return dir
	}
	// Even with an İndirgen file inside, these are never deleted.
	refused := []string{"/", mk(home, "config.json"), mk(filepath.Join(home, ".local", "share"), "config.json"),
		mk(filepath.Join(home, ".config"), "jobs.json"), mk(filepath.Join(home, ".cache"), "config.json"),
		mk(filepath.Join(home, ".local", "state"), "config.json"), mk(filepath.Join(home, ".local"), "config.json"),
		home + "/./",                                          // the same folder written differently
		mk(filepath.Join(home, "Belgeler")),                   // no İndirgen file
		mk(filepath.Join(home, "x"), "notes.txt"),             // someone else's files
		mk(filepath.Join(home, "y", "config.json", "inside")), // config.json is a folder
	}
	for _, d := range refused {
		if err := purgeAllowed(d); err == nil {
			t.Errorf("%s: allowed", d)
		}
	}
	t.Setenv("XDG_DATA_HOME", mk(xdgData, "config.json"))
	if err := purgeAllowed(xdgData); err == nil {
		t.Error("custom XDG_DATA_HOME allowed")
	}
	for _, d := range []string{mk(filepath.Join(xdgData, "indirgen"), "config.json", "token"),
		mk(filepath.Join(home, "veri"), "jobs.json")} {
		if err := purgeAllowed(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	if err := purgeAllowed(filepath.Join(home, "Belgeler")); err == nil || !strings.Contains(err.Error(), "silinmedi") {
		t.Fatalf("message: %v", err)
	}
}

// -purge never deletes the archive, even when it is inside the data folder
// or is the data folder itself, also when a symbolic link leads there.
func TestPurgeDataKeepsArchive(t *testing.T) {
	type tree map[string]string // relative path → content ("" = folder)
	build := func(t *testing.T, root string, files tree) {
		for p, body := range files {
			full := filepath.Join(root, filepath.FromSlash(p))
			if body == "" {
				os.MkdirAll(full, 0o755)
				continue
			}
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, []byte(body), 0o644)
		}
	}
	has := func(root, p string) bool {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		return err == nil
	}
	own := tree{"config.json": "{}", "jobs.json": "[]", "engines/yt-dlp/x/yt-dlp": "bin", "cookies/x.txt": "c",
		"parts/j1/a.part": "p", "temp/j2/b.mp4": "t", "archive/yt-dlp.txt": "a", ".config.json.123.tmp": "half"}

	t.Run("archive elsewhere", func(t *testing.T) {
		base := t.TempDir()
		data, lib := filepath.Join(base, "data"), filepath.Join(base, "lib")
		build(t, data, own)
		build(t, lib, tree{"Youtube/v.mkv": "v"})
		if kept, err := purgeData(data, lib); err != nil || kept != "" || has(data, "") || !has(lib, "Youtube/v.mkv") {
			t.Fatalf("kept=%q err=%v data=%v lib=%v", kept, err, has(data, ""), has(lib, "Youtube/v.mkv"))
		}
	})
	t.Run("archive inside", func(t *testing.T) {
		data := t.TempDir()
		lib := filepath.Join(data, "dl", "İndirgen")
		build(t, data, own)
		build(t, data, tree{"dl/İndirgen/Youtube/v.mkv": "v", "dl/İndirgen/empty": "", "dl/other.txt": "o"})
		kept, err := purgeData(data, lib)
		if err != nil || kept != lib {
			t.Fatalf("kept=%q err=%v", kept, err)
		}
		for _, p := range []string{"dl/İndirgen/Youtube/v.mkv", "dl/İndirgen/empty"} {
			if !has(data, p) {
				t.Errorf("archive lost %s", p)
			}
		}
		for p := range own {
			if has(data, p) {
				t.Errorf("%s kept", p)
			}
		}
		if has(data, "dl/other.txt") {
			t.Error("non-archive file beside the archive kept")
		}
	})
	t.Run("archive is the data folder", func(t *testing.T) {
		data := t.TempDir()
		build(t, data, own)
		build(t, data, tree{"Youtube/Kanal/v.mkv": "v", "Instagram/a.jpg": "i"})
		kept, err := purgeData(data, data+"/")
		if err != nil || kept != data+"/" {
			t.Fatalf("kept=%q err=%v", kept, err)
		}
		for p := range own {
			if has(data, p) {
				t.Errorf("%s kept", p)
			}
		}
		if !has(data, "Youtube/Kanal/v.mkv") || !has(data, "Instagram/a.jpg") {
			t.Error("archive files deleted")
		}
	})
	t.Run("symbolic links", func(t *testing.T) {
		base := t.TempDir()
		data := filepath.Join(base, "data")
		build(t, data, own)
		build(t, data, tree{"arsiv/v.mkv": "v"})
		// The configured archive is a link into the data folder.
		link := filepath.Join(base, "Arsivim")
		if err := os.Symlink(filepath.Join(data, "arsiv"), link); err != nil {
			t.Skip(err)
		}
		if kept, err := purgeData(data, link); err != nil || kept != link || !has(data, "arsiv/v.mkv") || has(data, "config.json") {
			t.Fatalf("archive behind a link: kept=%q err=%v", kept, err)
		}
		// The data folder is reached through a link, the archive by its real path.
		data2 := filepath.Join(base, "data2")
		build(t, data2, own)
		build(t, data2, tree{"dl/v.mkv": "v"})
		dataLink := filepath.Join(base, "veri")
		os.Symlink(data2, dataLink)
		if kept, err := purgeData(dataLink, filepath.Join(data2, "dl")); err != nil || kept == "" || !has(data2, "dl/v.mkv") || has(data2, "jobs.json") {
			t.Fatalf("data behind a link: kept=%q err=%v", kept, err)
		}
	})
	t.Run("archive inside but missing", func(t *testing.T) {
		data := t.TempDir()
		build(t, data, own)
		if kept, err := purgeData(data, filepath.Join(data, "dl", "yok")); err != nil || kept != "" || has(data, "") {
			t.Fatalf("kept=%q err=%v exists=%v", kept, err, has(data, ""))
		}
	})
}
