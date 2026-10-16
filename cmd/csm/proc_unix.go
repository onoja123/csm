//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

const (
	sigFailure = syscall.SIGUSR1
	sigSwitch  = syscall.SIGUSR2
)

func signalProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

func killProcessGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

func setUmask(mask int) int {
	return syscall.Umask(mask)
}

func detachedProcess() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
