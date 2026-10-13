package main

import (
	"syscall"
	"unsafe"
)

func isTerminal(fd uintptr) bool {
	var t syscall.Termios

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))

	return errno == 0
}

// setEcho turns the terminal's echo of typed characters on or off, so secrets can be typed without being shown.
func setEcho(fd uintptr, on bool) error {
	var t syscall.Termios

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}

	if on {
		t.Lflag |= syscall.ECHO
	} else {
		t.Lflag &^= syscall.ECHO
	}

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}

	return nil
}
