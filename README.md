# teams-recap-dl

Download a Teams meeting recording and its transcript from a Teams Recap link
or a SharePoint (Stream) video link. Each download gets its own folder, named
after the recording:

```text
<output dir>\<recording title>\
    <recording title>.mp4
    <recording title>.txt   [HH:MM:SS] speaker: text
    <recording title>.vtt   WebVTT subtitles with speaker tags
```

If the folder already exists, ` (1)`, ` (2)`, ... is appended.

## Usage

Double-click `teams-recap-dl.exe`, paste the URL, then pick the output folder.

Or from a terminal:

```powershell
teams-recap-dl.exe -out D:\recordings '<recording-url>'
teams-recap-dl.exe -transcript-only '<recording-url>'
teams-recap-dl.exe -no-transcript '<recording-url>'
```

`-h` lists all flags (`-browser`, `-profile`, `-ffmpeg`, `-wait`, `-debug`).

Requirements: Windows, Microsoft Edge, `ffmpeg` on `PATH` (not needed for `-transcript-only`).

## How it works

1. A Teams recap link is reduced to its `fileUrl` (the SharePoint player link).
2. Edge opens it with a dedicated profile (`%LOCALAPPDATA%\teams-recap-dl\profile`).
   On an Entra-joined PC sign-in is silent (PRT); otherwise sign in in the window.
3. Over CDP the tool captures the player's DASH manifest (`videomanifest`) and
   transcript (`cdnmedia/transcripts`) responses.
4. Both are SEA AES-128-CBC encrypted with the player's CDN key
   (`g_streamBootstrapContent.dashConfig.cdnDecryptionKey`, falling back to the
   manifest `keyUriTemplate`).
5. Segments are fetched with the browser's cookies and the player's live request
   headers (`X-Spopactoken`), decrypted, and muxed with `ffmpeg -c copy`.

The profile directory holds your session cookies; do not share it. URLs, tokens
and keys are never printed.

## Build

```powershell
go test ./...
go build -trimpath -ldflags "-s -w" -o teams-recap-dl.exe .
```

`chromedp` is pinned to v0.16.0; v0.24.0 uses a different (generic) API and does not build with this code.
