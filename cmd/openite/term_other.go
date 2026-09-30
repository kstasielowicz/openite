//go:build !windows && !linux && !darwin

package main

import (
	"errors"
	"os"
)

func isTerminal(f *os.File) bool { return false }
func enableVT()                  {}
func makeRaw() (func(), error)   { return nil, errors.New("raw terminal not supported") }
func termSize() (int, int)       { return 80, 24 }
