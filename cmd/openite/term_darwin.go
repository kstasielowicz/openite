//go:build darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func ioctl(fd uintptr, req uintptr, p unsafe.Pointer) bool {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(p))
	return e == 0
}

func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return ioctl(f.Fd(), syscall.TIOCGETA, unsafe.Pointer(&t))
}

func enableVT() {}

func makeRaw() (restore func(), err error) {
	var old syscall.Termios
	if !ioctl(os.Stdin.Fd(), syscall.TIOCGETA, unsafe.Pointer(&old)) {
		return nil, syscall.ENOTTY
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	ioctl(os.Stdin.Fd(), syscall.TIOCSETA, unsafe.Pointer(&raw))
	return func() { ioctl(os.Stdin.Fd(), syscall.TIOCSETA, unsafe.Pointer(&old)) }, nil
}

func termSize() (w, h int) {
	var ws struct{ row, col, x, y uint16 }
	if ioctl(os.Stdout.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) && ws.col > 0 {
		return int(ws.col), int(ws.row)
	}
	return 80, 24
}
