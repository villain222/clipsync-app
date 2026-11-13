package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// stubClipboard replaces the real clipboard accessors (which would spawn
// powershell/pbcopy and touch the actual OS clipboard) with in-memory ones.
// It returns a pointer to whatever was last "applied" so tests can assert on it.
func stubClipboard(t *testing.T) *string {
	t.Helper()
	origGet, origSet := getClipboard, setClipboard
	t.Cleanup(func() { getClipboard, setClipboard = origGet, origSet })
	applied := new(string)
	setClipboard = func(s string) error { *applied = s; return nil }
	getClipboard = func() (string, error) { return *applied, nil }
	return applied
}

func TestParseDoorbell(t *testing.T) {
	cases := []struct {
		payload string
		nonce   string
		port    string
		ok      bool
	}{
		{"CSYNC2|a1b2c3d4e5f60718|5001", "a1b2c3d4e5f60718", "5001", true},
		{"CSYNC2|deadbeef|6001", "deadbeef", "6001", true},
		{"some plain clipboard text", "", "", false}, // v1 raw data -> dropped
		{"CSYNC2|nonce", "", "", false},              // missing part
		{"CSYNC2||5001", "", "", false},              // empty nonce
		{"CSYNC2|nonce|", "", "", false},             // empty port
		{"CSYNC1|nonce|5000", "", "", false},         // wrong version
		{"", "", "", false},
	}
	for _, c := range cases {
		nonce, port, ok := parseDoorbell(c.payload)
		if ok != c.ok || nonce != c.nonce || port != c.port {
			t.Errorf("parseDoorbell(%q) = (%q,%q,%v); want (%q,%q,%v)",
				c.payload, nonce, port, ok, c.nonce, c.port, c.ok)
		}
	}
}

func TestStripTrailingShellNewline(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},             // no trailing newline -> unchanged
		{"abc\n", "abc"},           // pbpaste style: strip one
		{"abc\r\n", "abc"},         // powershell style: strip one CRLF
		{"abc\n\n", "abc\n"},       // user's trailing newline preserved
		{"abc\r\n\r\n", "abc\r\n"}, // user's trailing CRLF preserved
		{"", ""},                   // empty stays empty
		{"\n", ""},                 // only the shell newline -> empty
		{"\r\n", ""},               // only the shell CRLF -> empty
		{"a\nb\n", "a\nb"},         // internal newlines untouched
	}
	for _, c := range cases {
		if got := stripTrailingShellNewline(c.in); got != c.want {
			t.Errorf("stripTrailingShellNewline(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestBroadcastAddrs(t *testing.T) {
	addrs := broadcastAddrs()
	if len(addrs) == 0 {
		t.Fatal("expected at least the limited broadcast address")
	}
	// The limited broadcast must always be present as a fallback.
	found := false
	for _, a := range addrs {
		if a.Equal(net.IPv4bcast) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 255.255.255.255 in %v", addrs)
	}
	// Every address must be a valid 4-byte IPv4 and there must be no dupes.
	seen := make(map[string]bool)
	for _, a := range addrs {
		if a.To4() == nil {
			t.Errorf("non-IPv4 address in broadcast list: %v", a)
		}
		if seen[a.String()] {
			t.Errorf("duplicate broadcast address: %v", a)
		}
		seen[a.String()] = true
	}
}

func TestAEADRoundTrip(t *testing.T) {
	a, err := newAEAD("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newAEAD("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	nd, nl := make([]byte, 12), make([]byte, 12)
	nd[0], nl[0] = 1, 2
	msg := []byte("hello clipboard")
	ct := a.Seal(nil, nl, msg, aadFrom(nd, nl))
	plain, err := b.Open(nil, nl, ct, aadFrom(nd, nl))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if string(plain) != string(msg) {
		t.Fatalf("round trip mismatch: %q != %q", plain, msg)
	}
}

func TestWrongPINFailsToDecrypt(t *testing.T) {
	sender, err := newAEAD("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := newAEAD("wrong horse")
	if err != nil {
		t.Fatal(err)
	}
	nd, nl := make([]byte, 12), make([]byte, 12)
	ct := sender.Seal(nil, nl, []byte("secret"), aadFrom(nd, nl))
	if _, err := receiver.Open(nil, nl, ct, aadFrom(nd, nl)); err == nil {
		t.Fatal("expected GCM authentication to fail with a different PIN")
	}
}

// serveLoopback starts a TCP listener for the sender engine, opens a serve
// window, and runs the receiver's full pull handshake against it.
func serveLoopback(t *testing.T, sender, receiver *engine, content string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go sender.servePayload(c)
		}
	}()

	sender.sendMu.Lock()
	sender.activeSend = &sendSession{content: content, expiresAt: time.Now().Add(2 * time.Second)}
	sender.sendMu.Unlock()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	src := &net.UDPAddr{IP: net.ParseIP(host)}
	receiver.pullPayload(src, port)
}

func TestServePullLoopback(t *testing.T) {
	applied := stubClipboard(t)
	senderAEAD, _ := newAEAD("shared-pin")
	receiverAEAD, _ := newAEAD("shared-pin")
	sender := newEngine(&config{maxByte: 1 << 20, aead: senderAEAD})
	receiver := newEngine(&config{maxByte: 1 << 20, aead: receiverAEAD})

	content := "line one\nline two " + strings.Repeat("x", 5000)
	serveLoopback(t, sender, receiver, content)

	if *applied != content {
		t.Fatalf("receiver did not apply the payload; got %d bytes, want %d",
			len(*applied), len(content))
	}
	if receiver.lastContent == "" {
		t.Fatal("receiver lastContent was not updated")
	}
}

func TestServePullWrongPIN(t *testing.T) {
	applied := stubClipboard(t)
	senderAEAD, _ := newAEAD("correct-pin")
	receiverAEAD, _ := newAEAD("wrong-pin")
	sender := newEngine(&config{maxByte: 1 << 20, aead: senderAEAD})
	receiver := newEngine(&config{maxByte: 1 << 20, aead: receiverAEAD})

	content := "should not arrive"
	serveLoopback(t, sender, receiver, content)

	if *applied != "" {
		t.Fatalf("receiver with the wrong PIN applied content: %q", *applied)
	}
}

func TestServePullSizeCap(t *testing.T) {
	applied := stubClipboard(t)
	senderAEAD, _ := newAEAD("pin")
	receiverAEAD, _ := newAEAD("pin")
	sender := newEngine(&config{maxByte: 1 << 20, aead: senderAEAD})
	receiver := newEngine(&config{maxByte: 32, aead: receiverAEAD}) // tiny cap

	content := strings.Repeat("y", 1000) // far above the receiver's 32-byte cap
	serveLoopback(t, sender, receiver, content)

	if *applied != "" {
		t.Fatalf("oversized payload was applied despite the cap")
	}
}
