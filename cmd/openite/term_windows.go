package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	pGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	pSetConsoleMode = kernel32.NewProc("SetConsoleMode")
	pSetOutputCP    = kernel32.NewProc("SetConsoleOutputCP")
	pScreenBufInfo  = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

func isTerminal(f *os.File) bool {
	var m uint32
	r, _, _ := pGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&m)))
	return r != 0
}

// enableVT turns on ANSI colours/cursor control and UTF-8 output in the Windows console.
func enableVT() {
	pSetOutputCP.Call(65001)
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		var m uint32
		if r, _, _ := pGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&m))); r != 0 {
			pSetConsoleMode.Call(f.Fd(), uintptr(m|0x0004)) // ENABLE_VIRTUAL_TERMINAL_PROCESSING
		}
	}
}

// makeRaw switches stdin to unbuffered key input (arrow keys arrive as ANSI escape sequences).
func makeRaw() (restore func(), err error) {
	var m uint32
	if r, _, e := pGetConsoleMode.Call(os.Stdin.Fd(), uintptr(unsafe.Pointer(&m))); r == 0 {
		return nil, e
	}
	raw := (m &^ (0x0001 | 0x0002 | 0x0004)) | 0x0200 // no processed/line/echo input; VT input
	pSetConsoleMode.Call(os.Stdin.Fd(), uintptr(raw))
	return func() { pSetConsoleMode.Call(os.Stdin.Fd(), uintptr(m)) }, nil
}

func termSize() (w, h int) {
	var info struct {
		sizeX, sizeY             int16
		curX, curY               int16
		attr                     uint16
		left, top, right, bottom int16
		maxX, maxY               int16
	}
	if r, _, _ := pScreenBufInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info))); r != 0 {
		return int(info.right-info.left) + 1, int(info.bottom-info.top) + 1
	}
	return 80, 24
}
