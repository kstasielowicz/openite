package main

import "testing"

func TestParseTable(t *testing.T) {
	sample := "Name            Id                   Version  Available Source\n" +
		"--------------------------------------------------------------\n" +
		"Git             Git.Git              2.44.0   2.45.1    winget\n" +
		"Café Tool       Acme.Café            1.0      \n" +
		"2 upgrades available.\n"
	rows := parseTable(sample)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
	}
	if rows[0]["Id"] != "Git.Git" || rows[0]["Available"] != "2.45.1" {
		t.Errorf("bad row: %v", rows[0])
	}
	if rows[1]["Id"] != "Acme.Café" {
		t.Errorf("non-ASCII row shifted: %v", rows[1])
	}
}

func TestWingetOK(t *testing.T) {
	w := winget{}
	for _, c := range []int{0, 2316632107, -1978335189, 2316632161} {
		if !w.OK(c) {
			t.Errorf("code %d should be OK", c)
		}
	}
	if w.OK(1) {
		t.Error("exit 1 must fail")
	}
}
