//go:build !windows

// clipboard_unix.go
//
// macOS / Linux clipboard backend. Uses the platform's own clipboard tools:
// pbpaste / pbcopy on macOS, xclip on Linux (X11). The clipboard accessors
// are wired up in init(); clip_sync.go only declares the package-level
// getClipboard / setClipboard variables that tests may override.

package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func init() {
	getClipboard = unixGetClipboard
	setClipboard = unixSetClipboard
}

func unixGetClipboard() (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbpaste")
	case "linux":
		cmd = exec.Command("xclip", "-selection", "clipboard", "-o")
	default:
		return "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}

	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// Strip only the single trailing newline the command appends; the
	// clipboard content itself (including any user trailing newlines) is
	// preserved exactly.
	return stripTrailingShellNewline(string(out)), nil
}

func unixSetClipboard(content string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		cmd = exec.Command("xclip", "-selection", "clipboard", "-i")
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}

	cmd.Stdin = strings.NewReader(content)
	return cmd.Run()
}
