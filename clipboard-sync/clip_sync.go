// clip_sync.go
//
// Build:  go build -o clipsync.exe clip_sync.go
// Run:    .\clipsync.exe --enable      (from a terminal; Ctrl+C to stop)
//
// Run the SAME version on every machine. Older copies still running
// elsewhere will keep fighting with the fixed ones.

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"time"
)

const udpPort = 5000

var (
	// clipMu serializes every clipboard read/write and guards lastContent.
	// While a network update is being applied, the poll loop waits, so it
	// can never read a stale clipboard and broadcast the old value back.
	clipMu      sync.Mutex
	lastContent string
)

// Platform-specific clipboard getter
func getClipboard() (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("powershell", "-NoProfile", "-Command",
			"[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false; Get-Clipboard -Raw")
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
	// Trim trailing newlines from command output
	return strings.TrimRight(string(out), "\r\n"), nil
}

// Platform-specific clipboard setter
func setClipboard(content string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// Text is passed via stdin so spaces, quotes and symbols can't break the command
		cmd = exec.Command("powershell", "-NoProfile", "-Command",
			"[Console]::InputEncoding = New-Object System.Text.UTF8Encoding $false; $in = [Console]::In.ReadToEnd(); Set-Clipboard -Value $in")
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

// Short single-line preview for log output (keeps the terminal readable
// and avoids dumping full clipboard contents on screen).
func preview(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

// Start UDP sync server and local polling loop
func startSync() {
	// Listen for incoming UDP packets from other machines
	addr := &net.UDPAddr{
		Port: udpPort,
		IP:   net.IPv4zero,
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("Error listening on UDP port:", err)
		fmt.Println("(Is another copy already running?)")
		return
	}
	defer conn.Close()

	// Set up UDP broadcast destination
	broadcastAddr := &net.UDPAddr{
		IP:   net.IPv4bcast,
		Port: udpPort,
	}
	writeConn, err := net.DialUDP("udp", nil, broadcastAddr)
	if err != nil {
		fmt.Println("Error setting up UDP broadcast socket:", err)
	} else {
		defer writeConn.Close()
	}

	// Goroutine to handle incoming network messages
	go func() {
		buf := make([]byte, 65536)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			msg := string(buf[:n])
			if len(msg) == 0 {
				continue
			}

			clipMu.Lock()
			if msg == lastContent {
				clipMu.Unlock()
				continue
			}
			if err := setClipboard(msg); err != nil {
				fmt.Println("Error setting clipboard:", err)
				clipMu.Unlock()
				continue
			}
			// Record what the clipboard ACTUALLY holds now. The OS can
			// normalize text (e.g. \n becomes \r\n on Windows), and if
			// lastContent differs from that, the poll loop would think
			// the user copied something new and re-broadcast it.
			if actual, err := getClipboard(); err == nil && len(actual) > 0 {
				lastContent = actual
			} else {
				lastContent = msg
			}
			clipMu.Unlock()

			fmt.Printf("Synced from network: %s\n", preview(msg))
		}
	}()

	// Poll local clipboard every 300ms
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	fmt.Println("Clipboard sync running... Press Ctrl+C to stop.")
	for range ticker.C {
		clipMu.Lock()
		content, err := getClipboard()
		if err != nil || len(content) == 0 || content == lastContent {
			clipMu.Unlock()
			continue
		}
		lastContent = content
		clipMu.Unlock()

		if writeConn != nil {
			if _, err := writeConn.Write([]byte(content)); err != nil {
				fmt.Println("Error broadcasting clipboard:", err)
				continue
			}
			fmt.Printf("Sent: %s\n", preview(content))
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: clipsync --enable")
		fmt.Println("Press Ctrl+C to stop.")
		return
	}

	switch strings.TrimLeft(os.Args[1], "-") {
	case "enable":
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)
		go func() {
			<-sigChan
			fmt.Println("\nStopping clip-sync...")
			os.Exit(0)
		}()
		startSync()
	default:
		fmt.Println("Unknown option:", os.Args[1])
		fmt.Println("Usage: clipsync --enable")
	}
}