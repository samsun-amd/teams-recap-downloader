# teams-recap-downloader

Save a Teams meeting recording **and its transcript** to your PC — from the same
link you use to watch it.

```text
Paste link  ─▶  Edge signs you in  ─▶  <your folder>\<meeting title>\
                                          ├─ <meeting title>.mp4   video + audio
                                          ├─ <meeting title>.txt   transcript: [HH:MM:SS] speaker: text
                                          └─ <meeting title>.vtt   subtitles (open alongside the .mp4)
```

It works for recordings you can **play** in your browser, even when Teams does
not offer you a download. It does not give you access to
recordings you cannot already watch.

## Download

Get `teams-recap-dl.exe` from the
[latest release](https://github.com/samsun-amd/teams-recap-downloader/releases/latest).
It is a single file; no installation needed.

Windows may warn about an unrecognized app because the file is not code-signed:

- Edge download warning: open the download's **…** menu and choose **Keep**
  (confirm with **Keep anyway** if asked)
- SmartScreen dialog on first run: **More info** → **Run anyway**

## Requirements

- Windows 11 with **Microsoft Edge** (Windows 10 is untested)
- **ffmpeg** (to assemble the video; not needed for transcript-only). Install once:

  ```powershell
  winget install Gyan.FFmpeg
  ```

  Then open a new terminal so `ffmpeg` is on `PATH`.

## Quick start

1. Copy the recording link. Either kind works:
   - the Teams meeting **Recap** link (`https://teams.microsoft.com/l/meetingrecap?...`)
   - the recording's SharePoint / Stream link (`https://<company>-my.sharepoint.com/:v:/...`)
2. Double-click `teams-recap-dl.exe`.
3. Paste the link and press **Enter**.
4. Choose where to save in the folder dialog.
5. An **Edge window opens** and plays the recording muted. **Leave it open.**
   - On a company-managed PC you are usually signed in automatically.
   - Otherwise sign in (and complete MFA) in that window; it is remembered for next time.
6. Wait for `Saved video: ...`, then press **Enter** to close.

A 25-minute meeting typically takes a few minutes.

## Command line

```powershell
teams-recap-dl.exe -out D:\recordings '<link>'       # video + transcript
teams-recap-dl.exe -transcript-only '<link>'        # transcript only (fast, no ffmpeg)
teams-recap-dl.exe -no-transcript '<link>'          # video only
```

Put the link in **single quotes** — it contains `&` characters.
Without `-out`, files go to your `Downloads` folder.

| Option | Meaning |
|---|---|
| `-out <dir>` | Where to create the meeting folder |
| `-transcript-only` / `-no-transcript` | Download only one part |
| `-wait <min>` | How long to wait for sign-in and the player (default 10) |
| `-ffmpeg <path>` | ffmpeg location if it is not on `PATH` |
| `-browser <path>` | `msedge.exe` location if Edge is not found |
| `-profile <dir>` | Browser profile to use (default `%LOCALAPPDATA%\teams-recap-dl\profile`) |
| `-debug` | Print network diagnostics (contains file IDs — do not share publicly) |
| `-version` | Print the version |

If a folder with the same name exists, ` (1)`, ` (2)`, ... is added — nothing is overwritten.

## Troubleshooting

| Message / symptom | What to do |
|---|---|
| `ffmpeg not found` | Install it (see Requirements) and open a new terminal, or pass `-ffmpeg <path>` |
| `Still waiting for the player...` | Click **Play** in the Edge window; finish sign-in if a login page is shown |
| `timed out waiting for the player` | Check that you can play the recording in Edge yourself |
| `WARN: no transcript found` | The recording has no transcript; only the video is saved |
| `Edge failed to start` | Close any Edge window opened by the tool, then run again. Make sure you are on v1.0.1 or later (v1.0.0 failed this way when started by double-click) |
| `Closing a previous Edge window...` | Normal: an Edge from an earlier run was still open and is closed first |
| Signed in with the wrong account, or stuck at login | Close the tool, delete `%LOCALAPPDATA%\teams-recap-dl\profile`, run again |

Pressing **Ctrl+C** stops the download and closes the Edge window cleanly.

## Privacy

- The tool only talks to Microsoft (Teams / SharePoint); nothing is sent elsewhere.
- `%LOCALAPPDATA%\teams-recap-dl\profile` holds your sign-in session. Do not copy or share it.
- Links, tokens and keys are never printed. The saved files contain meeting content —
  share them only with people who could already watch the recording.

---

## How it works

1. A Teams recap link is reduced to its `fileUrl` (the SharePoint player link).
2. Edge opens it with a dedicated profile. On an Entra-joined PC sign-in is
   silent (Primary Refresh Token); otherwise the user signs in in the window.
3. Over CDP the tool captures the player's DASH manifest (`videomanifest`) and
   transcript (`cdnmedia/transcripts`) responses. Browser cache is disabled so
   these always reach the Network events, and the window is 1600×1000 so the
   transcript pane (which triggers the transcript request) is not collapsed.
4. Both are SEA AES-128-CBC encrypted with the player's CDN key
   (`g_streamBootstrapContent.dashConfig.cdnDecryptionKey`, falling back to the
   manifest `keyUriTemplate`).
5. Segments are fetched with the browser's cookies and the player's live request
   headers (`X-Spopactoken`), decrypted, and muxed with `ffmpeg -c copy`.
6. Edge is always closed gracefully (`Browser.close`, also on Ctrl+C / console
   close), so it never offers to "restore pages" on the next run. Before launch,
   an Edge still holding the profile `lockfile` is closed and a profile marked
   as crashed is reset. Edge is started with `--edge-skip-compat-layer-relaunch`:
   when the tool is launched from Explorer, Edge can otherwise relaunch itself
   and the process the tool started exits, which looks like a failed start.

## Build

Any OS with Go 1.26+ can build the Windows binary:

```bash
go test ./...
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o teams-recap-dl.exe .
```

`chromedp` is pinned to v0.16.0; v0.24.0 uses a different (generic) API and does
not build with this code. Windows `go.exe` cannot build from a `\\wsl.localhost`
path (file locking fails); build inside WSL instead.
