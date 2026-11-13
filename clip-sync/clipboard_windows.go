//go:build windows

// clipboard_windows.go
//
// Windows clipboard backend. It talks to the Win32 clipboard API directly
// (user32.dll for clipboard calls, kernel32.dll for Global* memory calls)
// instead of spawning a PowerShell process on every poll tick. That removes
// the per-check process-startup cost and the stdout-parsing fragility of the
// old shell approach.
//
// The clipboard accessors are wired up in init(); clip_sync.go only declares
// the package-level getClipboard / setClipboard variables that tests may
// override.

package main

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 clipboard constants.
const (
	cfUnicodeText = 13     // CF_UNICODETEXT
	gmemMoveable  = 0x0002 // GMEM_MOVEABLE
)

// Lazy procedure handles for the clipboard and global-memory APIs.
var (
	user32   = windows.NewLazyDLL("user32.dll")
	kernel32 = windows.NewLazyDLL("kernel32.dll")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procGetClipboardData = user32.NewProc("GetClipboardData")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
)

func init() {
	getClipboard = winGetClipboard
	setClipboard = winSetClipboard
}

// openClipboard retries OpenClipboard a few times with a short backoff.
// Windows intermittently returns ERROR_ACCESS_DENIED when the clipboard was
// just used by another (or a prior) call, and the documented remedy is to
// retry. The caller is responsible for CloseClipboard on success.
func openClipboard() error {
	const attempts = 5
	var lastErr error
	for i := 0; i < attempts; i++ {
		r, _, err := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		lastErr = err
		if i < attempts-1 {
			time.Sleep(time.Duration(10*(i+1)) * time.Millisecond)
		}
	}
	return fmt.Errorf("OpenClipboard: %w", lastErr)
}

// winGetClipboard reads the clipboard's text straight from the Win32
// clipboard. It returns ("", nil) when the clipboard is empty or holds no
// text format.
func winGetClipboard() (string, error) {
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call()

	r, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if r == 0 {
		return "", nil // empty clipboard, or no CF_UNICODETEXT present
	}
	hMem := r

	r, _, _ = procGlobalLock.Call(hMem)
	if r == 0 {
		return "", fmt.Errorf("GlobalLock failed (out of memory?)")
	}
	s := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(r)))
	procGlobalUnlock.Call(hMem)
	return s, nil
}

// winSetClipboard replaces the clipboard text. The freshly allocated global
// memory is handed to the system on success (which then owns it) and freed by
// us on any failure path.
func winSetClipboard(content string) error {
	u16, err := windows.UTF16FromString(content)
	if err != nil {
		return err
	}

	if err := openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	r, _, _ := procEmptyClipboard.Call()
	if r == 0 {
		return fmt.Errorf("EmptyClipboard failed")
	}

	// u16 includes a NUL terminator, so len(u16)*2 is exactly the size needed.
	r, _, _ = procGlobalAlloc.Call(gmemMoveable, uintptr(len(u16))*2)
	if r == 0 {
		return fmt.Errorf("GlobalAlloc failed (out of memory?)")
	}
	hMem := r

	r, _, _ = procGlobalLock.Call(hMem)
	if r == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("GlobalLock failed")
	}
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(r)), len(u16))
	copy(dst, u16)
	procGlobalUnlock.Call(hMem)

	r, _, _ = procSetClipboardData.Call(cfUnicodeText, hMem)
	if r == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("SetClipboardData failed")
	}
	return nil
}
