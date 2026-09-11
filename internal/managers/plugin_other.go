//go:build !linux

package managers

import "os/exec"

func setSysProcAttr(_ *exec.Cmd) {}
