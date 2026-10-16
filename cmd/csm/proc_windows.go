package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	sigFailure = syscall.Signal(0x1e)
	sigSwitch  = syscall.Signal(0x1f)
)

var errWindows = errors.New("csm does not support Windows yet")

func init() {
	fmt.Fprintln(os.Stderr, errWindows)
	os.Exit(1)
}

func signalProcess(int, syscall.Signal) error { return errWindows }

func killProcessGroup(int) {}

func processAlive(int) bool { return false }

func setUmask(int) int { return 0 }

func detachedProcess() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func lockFile(*os.File) error { return errWindows }
