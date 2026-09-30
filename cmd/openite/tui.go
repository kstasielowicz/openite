package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// A small dependency-free checkbox picker: arrows/space/enter, "/" to search, "a" to toggle a whole group.

type tuiItem struct {
	Key     string
	Label   string
	Note    string // shown dimmed after the label (e.g. "installed")
	Group   string
	Checked bool
	Dim     bool // visually de-emphasised (already installed) but still selectable
}

func readKey() string {
	var b [16]byte
	n, err := os.Stdin.Read(b[:])
	if err != nil || n == 0 {
		return "eof"
	}
	if b[0] == 0x1b {
		if n == 1 {
			return "esc"
		}
		if n >= 3 && (b[1] == '[' || b[1] == 'O') {
			switch b[2] {
			case 'A':
				return "up"
			case 'B':
				return "down"
			case 'H':
				return "home"
			case 'F':
				return "end"
			case '1', '7':
				return "home"
			case '4', '8':
				return "end"
			case '5':
				return "pgup"
			case '6':
				return "pgdn"
			}
		}
		return "?"
	}
	switch b[0] {
	case 0x03:
		return "ctrl-c"
	case '\r', '\n':
		return "enter"
	case ' ':
		return "space"
	case 0x7f, 0x08:
		return "backspace"
	}
	return string(b[:n])
}

func trunc(s string, w int) string {
	if w < 4 {
		w = 4
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}

// multiSelect shows the list and returns the checked keys (in list order). ok=false when cancelled.
func multiSelect(title, hint string, items []tuiItem) (keys []string, ok bool) {
	restore, err := makeRaw()
	if err != nil {
		return nil, false
	}
	fmt.Print("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	defer func() {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		restore()
	}()

	cursor, top, query, searching := 0, 0, "", false
	visible := func() []int { // indexes into items that match the search
		var v []int
		q := strings.ToLower(query)
		for i, it := range items {
			if q == "" || strings.Contains(strings.ToLower(it.Label+" "+it.Key+" "+it.Group), q) {
				v = append(v, i)
			}
		}
		return v
	}
	count := func() (n int) {
		for _, it := range items {
			if it.Checked {
				n++
			}
		}
		return
	}

	for {
		vis := visible()
		if cursor >= len(vis) {
			cursor = len(vis) - 1
		}
		if cursor < 0 {
			cursor = 0
		}
		w, h := termSize()
		// rows = visible items plus one header row wherever the group changes
		type row struct {
			header string
			idx    int // index into vis, -1 for headers
		}
		var rows []row
		cursorRow, last := 0, ""
		for vi, ii := range vis {
			if items[ii].Group != last && items[ii].Group != "" {
				rows = append(rows, row{header: items[ii].Group, idx: -1})
				last = items[ii].Group
			}
			if vi == cursor {
				cursorRow = len(rows)
			}
			rows = append(rows, row{idx: vi})
		}
		view := h - 8
		if view < 5 {
			view = 5
		}
		if cursorRow < top+1 { // keep the group header above the cursor visible too
			top = cursorRow - 1
		}
		if cursorRow >= top+view {
			top = cursorRow - view + 1
		}
		if top < 0 {
			top = 0
		}

		var sb strings.Builder
		sb.WriteString("\x1b[H")
		line := func(s string) { sb.WriteString(s + "\x1b[K\r\n") }
		line("")
		line("  " + accent("⬇ Openite") + "  " + bold(title))
		line("  " + dim(hint))
		if searching || query != "" {
			cur := ""
			if searching {
				cur = "▏"
			}
			line("  " + cyan("Search: ") + query + cur)
		} else {
			line("")
		}
		line("")
		for r := top; r < top+view; r++ {
			if r >= len(rows) {
				line("")
				continue
			}
			rw := rows[r]
			if rw.idx < 0 {
				line("  " + bold(rw.header))
				continue
			}
			it := items[vis[rw.idx]]
			box := "[ ]"
			if it.Checked {
				box = green("[✔]")
			}
			ptr := "  "
			if rw.idx == cursor {
				ptr = accent("❯ ")
			}
			label := trunc(it.Label, w-14-len(it.Note))
			if it.Dim {
				label = dim(label)
			}
			note := ""
			if it.Note != "" {
				note = " " + dim(it.Note)
			}
			line("  " + ptr + box + " " + label + note)
		}
		line("")
		line("  " + green(fmt.Sprintf("%d selected", count())) + dim("   ↑↓ move · space select · a whole group · / search · enter done · q quit"))
		sb.WriteString("\x1b[J")
		fmt.Print(sb.String())

		k := readKey()
		if searching {
			switch k {
			case "enter":
				searching = false
			case "esc":
				searching, query = false, ""
			case "backspace":
				if query != "" {
					r := []rune(query)
					query = string(r[:len(r)-1])
				}
			case "ctrl-c", "eof":
				return nil, false
			case "up", "down", "space":
				if k == "space" {
					query += " "
				}
			default:
				if utf8.RuneCountInString(k) == 1 && k[0] >= 0x20 {
					query += k
				}
			}
			cursor = 0
			continue
		}
		switch k {
		case "ctrl-c", "esc", "q", "eof":
			return nil, false
		case "up", "k":
			cursor--
		case "down", "j":
			cursor++
		case "pgup":
			cursor -= view
		case "pgdn":
			cursor += view
		case "home":
			cursor = 0
		case "end":
			cursor = len(vis) - 1
		case "space":
			if len(vis) > 0 {
				it := &items[vis[cursor]]
				it.Checked = !it.Checked
				cursor++
			}
		case "a":
			if len(vis) > 0 {
				g := items[vis[cursor]].Group
				all := true
				for _, ii := range vis {
					if items[ii].Group == g && !items[ii].Checked {
						all = false
					}
				}
				for _, ii := range vis {
					if items[ii].Group == g {
						items[ii].Checked = !all
					}
				}
			}
		case "/":
			searching = true
		case "enter":
			for _, it := range items {
				if it.Checked {
					keys = append(keys, it.Key)
				}
			}
			return keys, true
		}
	}
}
