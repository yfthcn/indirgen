package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Self-update
//
// Same rules as the engines: the release must publish SHA256SUMS, the new
// binary must match it and must run ("version" prints the new version)
// before it replaces the running one. The previous binary is kept next to
// it as <name>.old. Docker installs never self-update: the image is the
// unit of update there.
// ---------------------------------------------------------------------------

const (
	maxAppBinary = 100 << 20
	sumsAsset    = "SHA256SUMS"
)

// InstallMode tells the UI whether "Update" can be offered, and if not, why.
type InstallMode struct {
	Can    bool   `json:"can"`
	Reason string `json:"reason,omitempty"` // docker | readonly | dev
	Path   string `json:"path,omitempty"`   // the binary that would be replaced
}

// The binary's path is resolved once at start. Asking again after the swap
// is wrong on Linux: /proc/self/exe follows the renamed file, so a restart
// would launch <name>.old instead of the new binary.
var startExe, startExeErr = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}()

func runningExe() (string, error) { return startExe, startExeErr }

// inDocker is a variable so tests can run inside a container.
var inDocker = func() bool { return os.Getenv("INDIRGEN_DOCKER") == "1" || exists("/.dockerenv") }

// appAssetName is the release file for this platform (see build.sh).
func appAssetName(goos, goarch string) string { return project.Name + "-" + goos + "-" + goarch }

func (u *Updater) Mode() InstallMode {
	if inDocker() {
		return InstallMode{Reason: "docker"}
	}
	exe, err := u.exePath()
	if err != nil {
		return InstallMode{Reason: "readonly"}
	}
	// "go run" binaries live in a temp build dir: replacing them is pointless.
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return InstallMode{Reason: "dev", Path: exe}
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".indirgen-wtest-*")
	if err != nil {
		return InstallMode{Reason: "readonly", Path: exe}
	}
	f.Close()
	os.Remove(f.Name())
	return InstallMode{Can: true, Path: exe}
}

var errUpToDate = errors.New("zaten en son sürüm çalışıyor")

// Install downloads, verifies and swaps in the latest release. The caller
// restarts the process afterwards. It returns the installed version.
func (u *Updater) Install(ctx context.Context) (string, error) {
	if m := u.Mode(); !m.Can {
		return "", fmt.Errorf("bu kurulumda kendini güncelleme yok (%s)", m.Reason)
	}
	if !u.run.TryLock() {
		return "", errors.New("başka bir güncelleme işlemi sürüyor")
	}
	defer u.run.Unlock()
	u.set(func(s *UpdateState) { s.Installing, s.Error = true, "" })
	ver, err := u.install(ctx)
	u.set(func(s *UpdateState) {
		s.Installing = false
		if err != nil && !errors.Is(err, errUpToDate) {
			s.Error = firstLine(err.Error())
		}
	})
	if err != nil {
		slog.Warn("güncelleme kurulamadı", "hata", err)
	} else {
		slog.Info("güncelleme kuruldu, yeniden başlatılıyor", "eski", version, "yeni", ver)
	}
	return ver, err
}

func (u *Updater) install(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	rel, err := u.release(ctx)
	if err != nil {
		return "", err
	}
	newVer := strings.TrimPrefix(rel.Tag, "v")
	if !newerVersion(newVer, version) {
		return "", errUpToDate
	}
	name := appAssetName(runtime.GOOS, runtime.GOARCH)
	var bin, sums ghAsset
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			bin = a
		case sumsAsset:
			sums = a
		}
	}
	if bin.URL == "" {
		return "", fmt.Errorf("v%s içinde bu platform için dosya yok (%s)", newVer, name)
	}
	if sums.URL == "" {
		return "", fmt.Errorf("v%s içinde %s yok, doğrulanamayan sürüm kurulmaz", newVer, sumsAsset)
	}
	want, err := u.expectedSum(ctx, sums.URL, name)
	if err != nil {
		return "", err
	}

	exe, err := u.exePath()
	if err != nil {
		return "", err
	}
	tmp := exe + ".new"
	defer os.Remove(tmp) // no-op once renamed into place
	got, err := u.fetch(ctx, bin.URL, tmp)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf("SHA-256 uyuşmadı (beklenen %s, gelen %s)", want, got)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	// Smoke test: the new binary must start and report the new version.
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	out, err := exec.CommandContext(sctx, tmp, "version").CombinedOutput()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), project.Name+" "+newVer) {
		return "", fmt.Errorf("yeni sürüm çalışmadı: %v: %s", err, firstLine(string(out)))
	}

	// Swap: keep the current binary as .old. Renaming a running binary is
	// allowed; overwriting it in place is not ("text file busy").
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return "", fmt.Errorf("mevcut sürüm yedeklenemedi: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe) // put the working binary back
		return "", fmt.Errorf("yeni sürüm yerleştirilemedi: %w", err)
	}
	return newVer, nil
}

// expectedSum reads "<hash>  <name>" from a SHA256SUMS file.
func (u *Updater) expectedSum(ctx context.Context, url, name string) (string, error) {
	resp, err := httpGet(ctx, u.client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s içinde %s satırı yok", sumsAsset, name)
}

// fetch downloads url into dst and returns its SHA-256.
func (u *Updater) fetch(ctx context.Context, url, dst string) (string, error) {
	resp, err := httpGet(ctx, u.client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAppBinary+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxAppBinary {
		return "", errors.New("güncelleme dosyası çok büyük")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
