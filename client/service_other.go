//go:build !windows

package main

import (
	"log"
	"os"
	"os/exec"
)

// maybeRunAsWindowsService is a no-op on non-Windows platforms.
func maybeRunAsWindowsService(_ func()) bool {
	return false
}

func restartAgentService(reason string) {
	if err := exec.Command("sudo", "-n", "systemctl", "restart", "bwagent").Run(); err != nil {
		log.Printf("[%s] sudo systemctl restart 不可用(%v)，通过 os.Exit(0) 触发 systemd respawn", reason, err)
	}
	os.Exit(0)
}
