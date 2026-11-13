# clipsync - Uses standard keyboard Shortcuts!!!

A tiny clipboard-sync tool for Windows, macOS and Linux. Copy text on one machine on your local network and it shows up on the clipboard of every other machine running clipsync **with the same PIN**.

It is a small Go program (`clip_sync.go` plus one platform-specific clipboard file: `clipboard_windows.go` for Windows, `clipboard_unix.go` for macOS/Linux) with two small dependencies (`golang.org/x/term` for the no-echo PIN prompt, `golang.org/x/sys` for the Windows clipboard calls).

> **Read this first: the PIN is both the key and the membership check.**
> Every payload is encrypted (AES-256-GCM) with a key derived from a shared PIN you type at each startup. A machine that knows the PIN can read *and* overwrite your clipboard; a machine that does not know it can at most see that syncing is happening — it cannot decrypt the content. The key is derived from a short PIN, though, so someone who captures the traffic can still brute-force it offline: **use a long, random PIN** (e.g. `openssl rand -base64 24`), not a memorable phrase.

## How it works

Every running copy of clipsync does three things:

1. **Watches the local clipboard** (checked every 300 ms). When the text changes, it opens a short "serve window" and sends a tiny UDP **doorbell** to the broadcast address of every local network interface (plus `255.255.255.255` as a fallback) on port **5000**:
   ```
   CSYNC2|<nonce>|<tcpPort>
   ```
2. **Listens on UDP port 5000.** When a doorbell from another machine arrives, it dials that machine's TCP port and **pulls** the payload.
3. **Listens on TCP port 5001.** When a peer dials in to pull, it encrypts and sends the payload.

Only the doorbell travels over UDP (it carries no clipboard data). The actual text always moves over an encrypted TCP connection, keyed with `PBKDF2(PIN)`. A machine with the wrong PIN cannot decrypt — GCM authentication fails — and nothing is written to its clipboard.

| Concern   | Mechanism |
|-----------|-----------|
| Clipboard | Windows: direct Win32 clipboard API (no shell out); macOS `pbpaste` / `pbcopy`; Linux `xclip` |
| Discovery | UDP broadcast doorbell on port **5000** |
| Data      | Encrypted TCP on port **5001** |

## Requirements

- Go **1.24+** to build from source (the stdlib `crypto/pbkdf2` package used for key derivation requires it).
- All machines on the **same subnet** (same router/Wi-Fi). UDP broadcast does not cross routers, VPNs or most guest networks.
- UDP port **5000** and TCP port **5001** free and not blocked by a firewall.
- A shared sync PIN, agreed out-of-band (in person, a password manager, etc.).
- Linux only: `xclip` and an X11 session.

## Build

From the folder containing the source files:

```
go mod download            # fetches the x/term and x/sys dependencies
go build -o clipsync.exe . # Windows
go build -o clipsync .     # macOS or Linux, when building on that machine
```

### Cross-compiling from one machine to another

The code is pure Go, so you can build for any OS from any OS by setting `GOOS` and `GOARCH`.

**PowerShell:**

```powershell
$env:GOOS="linux";  $env:GOARCH="amd64"; go build -o clipsync-linux       .
$env:GOOS="darwin"; $env:GOARCH="arm64"; go build -o clipsync-mac-arm64   .
$env:GOOS="darwin"; $env:GOARCH="amd64"; go build -o clipsync-mac-amd64   .

# Reset so later builds target the current OS again
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
```

**cmd.exe** (different syntax, one command per line):

```
set GOOS=darwin
set GOARCH=arm64
go build -o clipsync-mac-arm64 .
set GOOS=
set GOARCH=
```

**macOS / Linux shell:**

```
GOOS=windows GOARCH=amd64 go build -o clipsync.exe .
```

Which macOS build do I need? Run `uname -m` on the Mac: `arm64` means Apple Silicon (use `darwin/arm64`), `x86_64` means Intel (use `darwin/amd64`).

## Usage

Run it from a terminal (do not double-click it, or the window will close immediately):

```
.\clipsync.exe                 # Windows -- prompts for the PIN (no echo)
.\clipsync.exe --pin mysecret  # ...or pass the PIN directly
./clipsync                     # macOS / Linux -- prompts for the PIN
```

You will be asked for the **same PIN on every machine**. You should then see `Clipboard sync running... Press Ctrl+C to stop.` Start it on every machine you want to sync, then copy some text on one of them. Log lines look like:

```
Sent: some text you copied...
Synced from network: text copied on another machine...
```

Only the first 40 characters are shown. Press **Ctrl+C** (or send SIGTERM) to stop.

### Flags

| Flag             | Default | Description |
|------------------|---------|-------------|
| `--pin <value>`  | *(prompt)* | Sync PIN. Omit to be prompted without echo. |
| `--udp-port <n>` | `5000`  | UDP discovery port. |
| `--tcp-port <n>` | `5001`  | TCP data port. |
| `--max-mb <f>`   | `2`     | Max payload in MiB, enforced on **send and receive**. |
| `--enable`       | —       | Accepted for backward compatibility; sync is always on. |

Tips:

- Run the **same version** on every machine. A v1 (pre-PIN) build ignores v2 doorbells and a v2 build ignores v1 packets — they simply will not sync with each other.
- Don't copy text out of the terminal window running clipsync while testing. That counts as a new copy and gets broadcast too.
- The PIN is never written to disk and must be re-entered at every startup.

## Security model

- **The PIN is the secret.** It is typed at each startup, used only to derive the AES key, and never stored. It gates both membership (wrong PIN ⇒ nothing applies) and confidentiality (wrong PIN ⇒ GCM auth fails).
- **Key derivation:** `PBKDF2-HMAC-SHA256(PIN, "clip-sync/v2", 120 000 iters)` → 256-bit key → AES-256-GCM. The salt and iteration count are public constants; the PIN is the only secret.
- **Per-connection nonces:** each TCP handshake generates fresh random nonces bound into the GCM additional-data, so a captured ciphertext cannot be replayed under a different handshake.
- **Because the key comes from a short PIN**, an attacker who captures traffic can brute-force it offline. That is the real threat model: protect the PIN, and treat any captured traffic as decryptable if the PIN is weak.

## v1 → v2 incompatibility

This is a breaking protocol change. If you were running a v1 build (no PIN, raw-UDP data, ~64 KB cap):

- v1 and v2 **cannot sync with each other**. v2 ignores v1's raw data packets, and v1 does not understand the `CSYNC2` doorbell.
- **Update every machine to v2** and restart, then pick a PIN and use it everywhere.
- The ~64 KB limit is gone: the default cap is now **2 MiB** (raise it with `--max-mb`).

## Platform setup

### Windows

On first run, Windows Defender Firewall asks whether to allow the app to communicate on the network. Click **Allow access** (private networks is enough). You now need both UDP **5000** and TCP **5001** allowed inbound. If you clicked Cancel or Deny, sending may still work but you will not receive anything. Fix it under *Windows Security > Firewall & network protection > Allow an app through firewall*, or delete the old firewall rule for clipsync and run it again to get the prompt back.

### macOS

macOS blocks or prompts for several things the first time an unsigned command-line tool does anything unusual. Work through these in order.

**1. Make the file executable**

```
chmod +x ./clipsync
```

**2. Remove the quarantine flag**

Files that arrive by browser download, AirDrop, chat or a ZIP are tagged as quarantined, and Gatekeeper will refuse to run them ("cannot be opened because the developer cannot be verified"). Clear the flag:

```
xattr -d com.apple.quarantine ./clipsync
```

If it says `No such xattr`, the file was not quarantined and you can move on. Files you built yourself or cloned with `git` are normally not quarantined.

**3. If you still get "Killed: 9" or "cannot be opened"**

Apple Silicon requires binaries to be code-signed. Go normally signs them with an ad-hoc signature, but if the signature was lost in transit, re-sign it:

```
codesign --force --sign - ./clipsync
```

Alternatively, open **System Settings > Privacy & Security**, scroll down to the Security section, and click **Open Anyway** for clipsync right after a blocked attempt. Or sidestep all of this by installing Go on the Mac and building there with `go build -o clipsync .`.

**4. Run it**

```
./clipsync
```

**5. Approve the privacy and network prompts**

Because clipsync sends/receives a UDP doorbell and accepts inbound TCP on your LAN, macOS may show these:

- **"Terminal would like to find and connect to devices on your local network"** (macOS Sequoia and later). Click **Allow**. The permission is granted to the app you launched clipsync from (Terminal, iTerm, VS Code and so on), not to clipsync itself. If you clicked Don't Allow, go to **System Settings > Privacy & Security > Local Network** and switch that terminal app on, then restart clipsync. Symptom of this being off: sending may appear to work, but nothing arrives from other machines.
- **"Do you want the application clipsync to accept incoming network connections?"** Click **Allow**. This only appears if the macOS firewall is on (**System Settings > Network > Firewall**). If you clicked Deny, open the Firewall settings, find clipsync in the list and set it to *Allow incoming connections*. This matters for the TCP data port (5001).
- **Pasteboard (clipboard) access prompts.** clipsync reads the clipboard through `pbpaste`. Apple has been developing a feature that asks permission when apps read the clipboard programmatically. As far as I know it is not enabled by default, but if your macOS version shows a prompt asking to allow clipboard access, choose **Allow**, otherwise clipsync cannot see what you copy.
- **"Paste blocked / Possible malware" in Terminal** (macOS 26.4 and later). This is a one-time warning when you paste commands into Terminal from another app, for example when copying the commands from this README. Click **Paste Anyway**. It is not related to clipsync itself.

**6. Keep it running while the Mac is in use**

clipsync only runs while its terminal window is open. If the Mac sleeps, syncing pauses and resumes when it wakes.

### Linux

Install `xclip` and run from within a graphical (X11) session:

```
sudo apt install xclip        # Debian / Ubuntu
./clipsync-linux
```

Notes:

- It will not work over plain SSH, because there is no display or clipboard to talk to.
- Wayland sessions are not supported. `xclip` is unreliable there, and the code would need to be changed to use `wl-clipboard`.
- If a firewall is active, allow UDP port 5000 and TCP port 5001 (for example `sudo ufw allow 5000/udp` and `sudo ufw allow 5001/tcp`).

## Troubleshooting

| Problem | Likely cause and fix |
|---------|----------------------|
| `Enter sync PIN:` then no sync | The PINs on the two machines differ, or one is still a v1 build. Use the identical PIN on both and confirm both are v2. |
| `Refused payload (N bytes) exceeds ... limit` | One side has a smaller `--max-mb` than the payload. Raise `--max-mb` on the receiving machine (and/or lower it on the sender). |
| `Skipped: clipboard (N bytes) exceeds ... limit` | You copied more than the sender's `--max-mb` allows. Raise `--max-mb` or copy less. |
| `Error binding UDP discovery port` | Port 5000 is in use, often because another copy of clipsync is already running. Close it. |
| `Error binding TCP data port` | Port 5001 is in use, or the firewall is blocking it. Free the port or allow it. |
| Sends but never receives | Firewall (Windows Defender / macOS Firewall) or macOS Local Network permission is blocking the TCP data port (5001) or the UDP doorbell (5000). See platform setup above. |
| Nothing syncs between two machines | They are on different subnets, one is on a guest or VPN network, or the router blocks client-to-client traffic ("AP isolation"). |
| One machine syncs, the other silently does nothing | The non-syncing machine has the wrong PIN (GCM auth fails and it applies nothing by design). |

## Limitations

- **Text only.** Images and files are not synced.
- **2 MiB maximum by default** per copy (raise with `--max-mb`). Enforced on both ends; oversized content is skipped with a clear message rather than a failed write.
- **UDP discovery is best-effort and broadcast-scoped.** The doorbell only reaches the local subnet; a lost doorbell means that one update is not pushed (the content stays retryable on the next local change).
- **Last write wins.** If two machines copy at nearly the same moment, the last one pulled ends up on the clipboard.
- **Polling uses a little CPU** (the clipboard is re-checked every 300 ms). On Windows this is now a direct Win32 clipboard read, so it no longer spawns a PowerShell process per check.
- **PIN-derived key is brute-forceable offline** if traffic is captured. Use a long, random PIN.
