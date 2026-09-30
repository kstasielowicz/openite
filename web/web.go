// Package web embeds the single-file UI and app icons shared by the Python server and `openite ui`.
package web

import "embed"

//go:embed index.html icons/*.svg
var Files embed.FS

// Index is the UI page.
var Index, _ = Files.ReadFile("index.html")
