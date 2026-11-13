// clip_sync.go
//
// A small clipboard-sync daemon. Two or more machines on the same LAN keep
// their clipboards in sync. Discovery is a tiny UDP "doorbell"; the actual
// payload travels over an encrypted TCP connection whose key is derived from
// a shared PIN you type at startup.
//
// Build:  go build -o clipsync.exe .          (Windows)
//         go build -o clipsync .              (macOS / Linux)
// Run:    .\clipsync.exe                (or .\clipsync.exe --enable)
//         You will be asked for the sync PIN (or pass --pin <value>).
//         Press Ctrl+C to stop.
//
// Flags:
//   --pin <value>    sync PIN (omit to be prompted without echo)
//   --udp-port <n>   UDP discovery port (default 5000)
//   --tcp-port <n>   TCP data port (default 5001)
//   --max-mb <f>     max payload in MiB, enforced on send AND receive (default 2)
//   --enable         accepted for backward compatibility (sync is always on)
//
// Protocol (v2):
//   1. Local clipboard changes -> the sender broadcasts a tiny UDP doorbell:
//         CSYNC2|<nonce>|<tcpPort>
//   2. Each peer that receives a doorbell dials the sender's TCP port and
//      pulls the payload.
//   3. Over TCP the dialer sends a 12-byte nonce, the sender replies with a
//      12-byte nonce, then sends a 4-byte plaintext length and an AES-256-GCM
//      ciphertext. The key is PBKDF2(PIN) -- so a machine with the wrong PIN
//      cannot decrypt the payload (GCM authentication fails) and nothing is
//      applied to its clipboard.
//
// Security:
//   * The PIN is never stored; it is typed at each startup and used only to
//     derive the AES key. It gates membership AND confidentiality.
//   * The key is derived from a short PIN, so a LAN attacker who captures
//     traffic can still brute-force it offline. Use a long, random PIN.
//
// v1 (raw-UDP) packets are ignored: only the CSYNC2 doorbell format is parsed.

package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	term "golang.org/x/term"
)

const (
	protocolVersion = "CSYNC2"
	defaultUDPPort  = 5000
	defaultTCPPort  = 5001
	defaultMaxMiB   = 2.0

	dialTimeout   = 3 * time.Second
	serveWindow   = 3 * time.Second // how long peers have to dial in after a doorbell
	connTimeout   = 15 * time.Second
	maxDoorbell   = 512
	gcmNonceBytes = 12
	gcmTagBytes   = 16
	pbkdf2Iter    = 120000
	keyBytes      = 32
	keySalt       = "clip-sync/v2"
)

// config holds immutable per-run settings.
type config struct {
	udpPort int
	tcpPort int
	maxByte int64
	aead    cipher.AEAD
}

// engine holds the mutable sync state.
type engine struct {
	cfg *config

	// clipMu serializes clipboard reads/writes and guards lastContent. While a
	// network update is being applied, the poll loop cannot read a stale
	// clipboard and broadcast the old value back (prevents echo loops,
	// including the Windows \n -> \r\n normalization case).
	clipMu      sync.Mutex
	lastContent string

	// activeSend is the payload offered to dialers during a serve window.
	sendMu     sync.Mutex
	activeSend *sendSession

	// dedupSeen maps "ip:port" -> last doorbell nonce, to ignore UDP duplicates.
	dedupMu   sync.Mutex
	dedupSeen map[string]string
}

// sendSession is a single "I have new content, come get it" window.
type sendSession struct {
	content   string
	expiresAt time.Time
	mu        sync.Mutex
	served    int // number of successful transfers in this window
}

func newEngine(cfg *config) *engine {
	return &engine{
		cfg:       cfg,
		dedupSeen: make(map[string]string),
	}
}

// --- Clipboard access ------------------------------------------------------
//
// getClipboard and setClipboard are the active clipboard backends. They are
// assigned in init() by the platform file (clipboard_windows.go on Windows,
// clipboard_unix.go everywhere else) and may be overridden by tests. Keeping
// them as package-level variables is the seam the test suite stubs out.

var (
	getClipboard func() (string, error)
	setClipboard func(content string) error
)

// stripTrailingShellNewline removes the single trailing newline that the
// platform command appends when writing to the pipe, without eating newlines
// the user actually placed at the end of the clipboard content. (pbpaste adds
// "\n"; PowerShell adds "\r\n"; xclip adds nothing.) Stripping exactly one
// keeps "abc\n" intact while still removing the shell's own terminator.
func stripTrailingShellNewline(s string) string {
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "\n"), strings.HasSuffix(s, "\r"):
		return s[:len(s)-1]
	}
	return s
}

// preview is a short single-line rendering for log output (keeps the terminal
// readable and avoids dumping full clipboard contents to the screen).
func preview(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

// --- Key derivation and crypto --------------------------------------------

func newAEAD(pin string) (cipher.AEAD, error) {
	// PBKDF2 stretches a short PIN into a 256-bit key and slows offline
	// brute-force of captured traffic. Both sides use the same public salt and
	// parameters, so identical PINs yield identical keys.
	key, err := pbkdf2.Key(sha256.New, pin, []byte(keySalt), pbkdf2Iter, keyBytes)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// aadFrom binds both nonces into the GCM additional-data so a ciphertext
// cannot be replayed under a different handshake.
func aadFrom(nd, nl []byte) []byte {
	a := make([]byte, 0, len(nd)+len(nl))
	a = append(a, nd...)
	a = append(a, nl...)
	return a
}

func readFull(c net.Conn, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// --- Doorbell (UDP discovery) ---------------------------------------------

func (e *engine) listenUDP() {
	addr := &net.UDPAddr{Port: e.cfg.udpPort, IP: net.IPv4zero}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("Error binding UDP discovery port:", err)
		fmt.Println("(Is another copy already running on this machine?)")
		return
	}
	defer conn.Close()

	buf := make([]byte, maxDoorbell)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		e.handleDoorbell(string(buf[:n]), src)
	}
}

// parseDoorbell validates and splits a v2 doorbell payload of the form
// "CSYNC2|<nonce>|<tcpPort>". It returns ok=false for anything else, which is
// how v1 raw-UDP data and noise are dropped.
func parseDoorbell(payload string) (nonce, tcpPort string, ok bool) {
	parts := strings.Split(payload, "|")
	if len(parts) != 3 || parts[0] != protocolVersion {
		return "", "", false
	}
	nonce, tcpPort = parts[1], parts[2]
	if nonce == "" || tcpPort == "" {
		return "", "", false
	}
	return nonce, tcpPort, true
}

func (e *engine) handleDoorbell(payload string, src *net.UDPAddr) {
	nonce, tcpPort, ok := parseDoorbell(payload)
	if !ok {
		// Not a v2 doorbell (v1 raw data, or noise) -- ignore.
		return
	}

	key := src.String()
	e.dedupMu.Lock()
	if e.dedupSeen[key] == nonce {
		e.dedupMu.Unlock()
		return // duplicate doorbell (UDP can deliver the same datagram twice)
	}
	e.dedupSeen[key] = nonce
	e.dedupMu.Unlock()

	// Dial back to the sender's TCP port and pull the payload.
	go e.pullPayload(src, tcpPort)
}

// --- Pull (we are the receiver; we dial the sender) ------------------------

func (e *engine) pullPayload(src *net.UDPAddr, tcpPort string) {
	addr := net.JoinHostPort(src.IP.String(), tcpPort)
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return // sender not reachable or not listening; nothing to pull
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))

	// Handshake: we are the dialer. Send our 12-byte nonce.
	nd := make([]byte, gcmNonceBytes)
	if _, err := rand.Read(nd); err != nil {
		return
	}
	if _, err := conn.Write(nd); err != nil {
		return
	}
	// Sender replies with its 12-byte nonce.
	nl, err := readFull(conn, gcmNonceBytes)
	if err != nil {
		return
	}
	// Sender sends the plaintext length.
	lb, err := readFull(conn, 4)
	if err != nil {
		return
	}
	plainLen := int64(binary.BigEndian.Uint32(lb))
	if plainLen == 0 || plainLen > e.cfg.maxByte {
		fmt.Printf("Refused payload (%d bytes) exceeds %d MiB limit\n",
			plainLen, e.cfg.maxByte/1024/1024)
		return
	}
	// Sender sends GCM ciphertext (len) || tag (16).
	ct, err := readFull(conn, int(plainLen)+gcmTagBytes)
	if err != nil {
		return
	}
	plain, err := e.cfg.aead.Open(nil, nl, ct, aadFrom(nd, nl))
	if err != nil {
		// Wrong PIN (or a tampered packet): GCM authentication fails. Say
		// nothing that reveals which -- just don't apply it.
		return
	}
	e.applyReceived(string(plain))
}

// applyReceived puts pulled content into the local clipboard under the lock,
// then records what the clipboard ACTUALLY holds (the OS may normalize text).
func (e *engine) applyReceived(content string) {
	e.clipMu.Lock()
	if content == e.lastContent {
		e.clipMu.Unlock()
		return
	}
	if err := setClipboard(content); err != nil {
		e.clipMu.Unlock()
		fmt.Println("Error setting clipboard:", err)
		return
	}
	if actual, err := getClipboard(); err == nil && len(actual) > 0 {
		e.lastContent = actual
	} else {
		e.lastContent = content
	}
	e.clipMu.Unlock()

	fmt.Printf("Synced from network: %s\n", preview(content))
}

// --- Serve (we are the sender; a peer dials in) ----------------------------

func (e *engine) listenTCP() {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", e.cfg.tcpPort))
	if err != nil {
		fmt.Println("Error binding TCP data port:", err)
		return
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go e.servePayload(conn)
	}
}

func (e *engine) servePayload(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))

	// We are the listener (sender). Read the dialer's 12-byte nonce.
	nd, err := readFull(conn, gcmNonceBytes)
	if err != nil {
		return
	}
	// Look up the active send window (the payload to serve).
	e.sendMu.Lock()
	sess := e.activeSend
	if sess == nil || time.Now().After(sess.expiresAt) {
		e.sendMu.Unlock()
		return // no fresh content to serve (stale dial / window closed)
	}
	content := sess.content
	e.sendMu.Unlock()

	// Reply with our 12-byte nonce.
	nl := make([]byte, gcmNonceBytes)
	if _, err := rand.Read(nl); err != nil {
		return
	}
	if _, err := conn.Write(nl); err != nil {
		return
	}
	// Send the plaintext length.
	lb := make([]byte, 4)
	binary.BigEndian.PutUint32(lb, uint32(len(content)))
	if _, err := conn.Write(lb); err != nil {
		return
	}
	// Encrypt and send ciphertext || tag.
	ct := e.cfg.aead.Seal(nil, nl, []byte(content), aadFrom(nd, nl))
	if _, err := conn.Write(ct); err != nil {
		return
	}

	sess.mu.Lock()
	sess.served++
	sess.mu.Unlock()
}

// --- Broadcast (we detected a local change) --------------------------------

// broadcastAddrs returns the IPv4 broadcast addresses the doorbell should be
// sent to: one per up, non-loopback interface that has an IPv4 address, plus
// the limited broadcast 255.255.255.255 as a universal fallback. Sending to
// each interface's own subnet (instead of only the default one) is what makes
// discovery work on machines with more than one network interface.
func broadcastAddrs() []net.IP {
	seen := make(map[string]bool)
	var out []net.IP
	add := func(ip net.IP) {
		if !seen[ip.String()] {
			seen[ip.String()] = true
			out = append(out, ip)
		}
	}

	add(net.IPv4bcast) // 255.255.255.255

	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			mask := ipnet.Mask
			if ip4 == nil || len(mask) != 4 {
				continue
			}
			// Broadcast address = network address with the host bits set.
			bcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bcast[i] = (ip4[i] & mask[i]) | ^mask[i]
			}
			add(bcast)
		}
	}
	return out
}

func (e *engine) broadcast(content string) {
	// Open a serve window so peers can dial in and pull the payload.
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		// rand.Read failing is effectively impossible; fall back to time.
		binary.BigEndian.PutUint64(nonceBytes, uint64(time.Now().UnixNano()))
	}
	nonce := hex.EncodeToString(nonceBytes)
	sess := &sendSession{content: content, expiresAt: time.Now().Add(serveWindow)}

	e.sendMu.Lock()
	e.activeSend = sess
	e.sendMu.Unlock()

	// After the window, if nobody pulled it, make it retryable on the next tick.
	go func() {
		time.Sleep(serveWindow)
		e.sendMu.Lock()
		if e.activeSend == sess {
			e.activeSend = nil
		}
		e.sendMu.Unlock()

		sess.mu.Lock()
		served := sess.served
		sess.mu.Unlock()
		if served == 0 {
			e.clipMu.Lock()
			if e.lastContent == content {
				e.lastContent = "" // retry: let the poll loop re-broadcast
			}
			e.clipMu.Unlock()
		}
	}()

	// Doorbell: send the same tiny payload to every local broadcast address
	// (multi-interface friendly) so peers on any of our subnets dial in.
	payload := fmt.Sprintf("%s|%s|%d", protocolVersion, nonce, e.cfg.tcpPort)
	data := []byte(payload)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0})
	if err != nil {
		fmt.Println("Error setting up UDP socket:", err)
		e.resetIfCurrent(content)
		return
	}
	defer conn.Close()

	sent := false
	for _, bcastIP := range broadcastAddrs() {
		if _, werr := conn.WriteToUDP(data, &net.UDPAddr{IP: bcastIP, Port: e.cfg.udpPort}); werr == nil {
			sent = true
		}
	}
	if !sent {
		fmt.Println("Error broadcasting doorbell (no interface accepted it)")
		e.resetIfCurrent(content)
		return
	}
	fmt.Printf("Sent: %s\n", preview(content))
}

// resetIfCurrent clears lastContent for content if it is still the current
// value, so a failed send is retried on the next poll tick.
func (e *engine) resetIfCurrent(content string) {
	e.clipMu.Lock()
	if e.lastContent == content {
		e.lastContent = ""
	}
	e.clipMu.Unlock()
}

// --- Main loop -------------------------------------------------------------

func (e *engine) run() {
	go e.listenUDP()
	go e.listenTCP()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	fmt.Println("Clipboard sync running... Press Ctrl+C to stop.")

	for range ticker.C {
		e.clipMu.Lock()
		content, err := getClipboard()
		if err != nil || len(content) == 0 || content == e.lastContent {
			e.clipMu.Unlock()
			continue
		}
		if int64(len(content)) > e.cfg.maxByte {
			// Record it as seen (so we don't re-flag every tick) but do not
			// broadcast: it's too large to sync.
			e.lastContent = content
			e.clipMu.Unlock()
			fmt.Printf("Skipped: clipboard (%d bytes) exceeds the %d MiB sync limit\n",
				len(content), e.cfg.maxByte/1024/1024)
			continue
		}
		e.lastContent = content
		e.clipMu.Unlock()

		e.broadcast(content)
	}
}

// --- Startup ----------------------------------------------------------------

func readPin() (string, error) {
	fmt.Print("Enter sync PIN: ")
	p, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		// Fallback for non-terminal stdin (piped input, CI, etc.): read a line.
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			return strings.TrimRight(sc.Text(), "\r\n"), nil
		}
		return "", fmt.Errorf("read PIN: %w (you can also pass --pin <value>)", err)
	}
	return string(p), nil
}

func main() {
	pinFlag := flag.String("pin", "", "sync PIN (omit to be prompted without echo)")
	enable := flag.Bool("enable", true, "accepted for backward compatibility (sync is always on)")
	udpPort := flag.Int("udp-port", defaultUDPPort, "UDP discovery port")
	tcpPort := flag.Int("tcp-port", defaultTCPPort, "TCP data port")
	maxMB := flag.Float64("max-mb", defaultMaxMiB, "max payload in MiB (send and receive)")
	flag.Parse()
	_ = enable

	pin := *pinFlag
	if pin == "" {
		p, err := readPin()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		pin = p
	}
	if strings.TrimSpace(pin) == "" {
		fmt.Println("PIN must not be empty.")
		os.Exit(1)
	}

	aead, err := newAEAD(pin)
	if err != nil {
		fmt.Println("Error deriving key:", err)
		os.Exit(1)
	}

	maxByte := int64(*maxMB * 1024 * 1024)
	if maxByte <= 0 {
		maxByte = int64(defaultMaxMiB * 1024 * 1024)
	}

	cfg := &config{
		udpPort: *udpPort,
		tcpPort: *tcpPort,
		maxByte: maxByte,
		aead:    aead,
	}

	e := newEngine(cfg)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nStopping clip-sync...")
		os.Exit(0)
	}()

	e.run()
}
