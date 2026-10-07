//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package plugin

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}

func signalProcessGroup(cmd *exec.Cmd, kill bool) error { return cmd.Process.Kill() }
