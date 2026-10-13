//go:build windows

package stream

import (
	"os/exec"
	"syscall"
)

// hideWindow stops ffmpeg from flashing a console window when running as a
// desktop app or service on Windows.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
