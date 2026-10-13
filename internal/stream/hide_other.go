//go:build !windows

package stream

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
