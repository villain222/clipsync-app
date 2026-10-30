clipsync

A tiny clipboard sync tool for Windows, macOS and Linux. Copy text on one machine on your local network and it shows up on the clipboard of every other machine running clipsync.

It is a single Go source file (clip_sync.go) with no third-party dependencies.

    Read this first: there is no encryption or authentication. Everything you copy, including passwords and tokens, is broadcast in plain text to every device on your local network. Anyone on that network can also overwrite your clipboard. Only use clipsync on a network you fully trust (for example, your home network), never on public Wi-Fi, and stop it when you are copying anything sensitive.

How it works

Every running copy of clipsync does two things:

    Watches the local clipboard (checked every 300 ms). When the text changes, it is broadcast over UDP to 255.255.255.255:5000.
    Listens on UDP port 5000. When text arrives from another machine, it is written to the local clipboard.

Clipboard access uses each OS's own tools:
OS 	Read 	Write
Windows 	PowerShell Get-Clipboard 	PowerShell Set-Clipboard
macOS 	pbpaste 	pbcopy
Linux 	xclip -o 	xclip -i
Requirements

    Go (to build from source). Any recent version should work.
    All machines on the same subnet (same router/Wi-Fi network). UDP broadcast does not cross routers, VPNs or most guest networks.
    UDP port 5000 must be free and not blocked by a firewall.
    Linux only: xclip and an X11 session.

Build

From the folder containing clip_sync.go:

go build -o clipsync.exe clip_sync.go      # Windows
go build -o clipsync clip_sync.go          # macOS or Linux, when building on that machine

Cross-compiling from one machine to another

The code is pure Go, so you can build for any OS from any OS by setting GOOS and GOARCH.

PowerShell:

$env:GOOS="linux";  $env:GOARCH="amd64"; go build -o clipsync-linux       clip_sync.go
$env:GOOS="darwin"; $env:GOARCH="arm64"; go build -o clipsync-mac-arm64   clip_sync.go
$env:GOOS="darwin"; $env:GOARCH="amd64"; go build -o clipsync-mac-amd64   clip_sync.go

# Reset so later builds target the current OS again
Remove-Item Env:GOOS; Remove-Item Env:GOARCH

cmd.exe (different syntax, one command per line):

set GOOS=darwin
set GOARCH=arm64
go build -o clipsync-mac-arm64 clip_sync.go
set GOOS=
set GOARCH=

macOS / Linux shell:

GOOS=windows GOARCH=amd64 go build -o clipsync.exe clip_sync.go

Which macOS build do I need? Run uname -m on the Mac: arm64 means Apple Silicon (use darwin/arm64), x86_64 means Intel (use darwin/amd64).
Usage

Run it from a terminal (do not double-click it, or the window will close immediately):

clipsync.exe --enable          # Windows
./clipsync --enable            # macOS / Linux

You should see Clipboard sync running... Press Ctrl+C to stop. Start it on every machine you want to sync, then copy some text on one of them. Log lines look like:

Sent: some text you copied...
Synced from network: text copied on another machine...

Only the first 40 characters are shown. Press Ctrl+C to stop.

Tips:

    Run the same version on every machine. Older builds can fight with newer ones and cause clipboards to flip back and forth.
    Don't copy text out of the terminal window running clipsync while testing. That counts as a new copy and gets broadcast too.

Platform setup
Windows

On first run, Windows Defender Firewall asks whether to allow the app to communicate on the network. Click Allow access (private networks is enough). If you clicked Cancel or Deny, sending still works but you will not receive anything. Fix it under Windows Security > Firewall & network protection > Allow an app through firewall, or delete the old firewall rule for clipsync and run it again to get the prompt back.
macOS

macOS blocks or prompts for several things the first time an unsigned command-line tool does anything unusual. Work through these in order.

1. Make the file executable

chmod +x ./clipsync-mac-arm64

2. Remove the quarantine flag

Files that arrive by browser download, AirDrop, chat or a ZIP are tagged as quarantined, and Gatekeeper will refuse to run them ("cannot be opened because the developer cannot be verified"). Clear the flag:

xattr -d com.apple.quarantine ./clipsync-mac-arm64

If it says No such xattr, the file was not quarantined and you can move on. Files you built yourself or cloned with git are normally not quarantined.

3. If you still get "Killed: 9" or "cannot be opened"

Apple Silicon requires binaries to be code-signed. Go normally signs them with an ad-hoc signature, but if the signature was lost in transit, re-sign it:

codesign --force --sign - ./clipsync-mac-arm64

Alternatively, open System Settings > Privacy & Security, scroll down to the Security section, and click Open Anyway for clipsync right after a blocked attempt. Or sidestep all of this by installing Go on the Mac and building there with go build -o clipsync clip_sync.go.

4. Run it

./clipsync-mac-arm64 --enable

5. Approve the privacy and network prompts

Because clipsync sends and receives UDP broadcasts on your LAN, macOS may show these:

    "Terminal would like to find and connect to devices on your local network" (macOS Sequoia and later). Click Allow. The permission is granted to the app you launched clipsync from (Terminal, iTerm, VS Code and so on), not to clipsync itself. If you clicked Don't Allow, go to System Settings > Privacy & Security > Local Network and switch that terminal app on, then restart clipsync. Symptom of this being off: sending may appear to work, but nothing arrives from other machines.
    "Do you want the application clipsync to accept incoming network connections?" Click Allow. This only appears if the macOS firewall is on (System Settings > Network > Firewall). If you clicked Deny, open the Firewall settings, find clipsync in the list and set it to Allow incoming connections.
    Pasteboard (clipboard) access prompts. clipsync reads the clipboard through pbpaste. Apple has been developing a feature that asks permission when apps read the clipboard programmatically. As far as I know it is not enabled by default, but if your macOS version shows a prompt asking to allow clipboard access, choose Allow, otherwise clipsync cannot see what you copy.
    "Paste blocked / Possible malware" in Terminal (macOS 26.4 and later). This is a one-time warning when you paste commands into Terminal from another app, for example when copying the commands from this README. Click Paste Anyway. It is not related to clipsync itself.

6. Keep it running while the Mac is in use

clipsync only runs while its terminal window is open. If the Mac sleeps, syncing pauses and resumes when it wakes.
Linux

Install xclip and run from within a graphical (X11) session:

sudo apt install xclip        # Debian / Ubuntu
./clipsync-linux --enable

Notes:

    It will not work over plain SSH, because there is no display or clipboard to talk to.
    Wayland sessions are not supported. xclip is unreliable there, and the code would need to be changed to use wl-clipboard.
    If a firewall is active, allow UDP port 5000 (for example sudo ufw allow 5000/udp).

Troubleshooting
Problem 	Likely cause and fix
Window flashes and closes (Windows) 	Run it from a terminal with --enable. With no argument it prints usage and exits.
Error listening on UDP port 	Port 5000 is in use, often because another copy of clipsync is already running. Close it.
Sends but never receives 	Firewall (Windows Defender / macOS Firewall) or macOS Local Network permission is blocking incoming packets. See platform setup above.
Nothing syncs between two machines 	They are on different subnets, one is on a guest or VPN network, or the router blocks client-to-client traffic ("AP isolation").
Clipboards keep flipping between machines 	Some machines are running an old build. Update and restart clipsync everywhere.
Error setting clipboard: exit status 1 on Windows 	You are running an old build that passed text on the PowerShell command line. Rebuild from the current source.
Error setting clipboard on Linux 	xclip is not installed, or there is no X11 display available.
Accented characters or emoji look wrong 	Make sure every machine runs the current build, which handles UTF-8 explicitly.
Limitations

    Text only. Images and files are not synced.
    About 64 KB maximum per copy (the UDP packet limit). Larger clips will fail or be cut off.
    UDP is best-effort. A packet can be lost, and there is no retry.
    Last write wins. If two machines copy at nearly the same moment, whichever packet arrives last ends up on the clipboard.
    Polling uses CPU, especially on Windows where every check starts a PowerShell process.
    No encryption, authentication or machine allow-list. See the warning at the top.
    Broadcast only reaches the local subnet.
