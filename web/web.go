// Package web embeds the single-file UI shared by the Python server and `openite ui`.
package web

import _ "embed"

//go:embed index.html
var Index []byte
