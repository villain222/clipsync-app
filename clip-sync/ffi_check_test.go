//go:build windows

package main

import (
	"strings"
	"testing"
)

// TestWinClipboardRealFFI exercises the real Win32 clipboard backend (the
// other unit tests stub it out). It saves the current clipboard, does a
// set->get round trip through the actual user32/kernel32 calls for a range of
// content, then restores the original.
func TestWinClipboardRealFFI(t *testing.T) {
	orig, err := getClipboard()
	if err != nil {
		t.Fatalf("read original clipboard: %v", err)
	}
	defer func() { _ = setClipboard(orig) }() // always restore

	markers := []string{
		"clipsync-ffi-check-12345\nsecond line",
		"plain ASCII",
		"unicode: café ☕ 中文 — 你好",
		"trailing newlines\n\n\n", // the old TrimRight bug ate these
		"tab\tand\nmultiple\nlines",
		strings.Repeat("A", 256*1024), // 256 KiB payload
	}

	for i, marker := range markers {
		if err := setClipboard(marker); err != nil {
			t.Fatalf("case %d setClipboard: %v", i, err)
		}
		got, err := getClipboard()
		if err != nil {
			t.Fatalf("case %d getClipboard: %v", i, err)
		}
		if got != marker {
			t.Fatalf("case %d round trip mismatch:\n got  %q\n want %q", i, got, marker)
		}
	}
}
