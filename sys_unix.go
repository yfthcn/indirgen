//go:build linux || darwin || freebsd

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setProcAttrs runs engines in their own process group so cancelling a job
// also stops ffmpeg/deno children spawned by yt-dlp.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 8 * time.Second
}

// diskFree returns available bytes for dir (or its nearest existing parent), -1 if unknown.
func diskFree(dir string) int64 {
	for range 8 {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err == nil {
			return int64(uint64(st.Bavail) * uint64(st.Bsize))
		}
		dir = parentDir(dir)
	}
	return -1
}

// reexec replaces this process with the (updated) binary, same PID and
// arguments: systemd and shells see one continuous process.
func reexec() error {
	exe, err := runningExe()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
