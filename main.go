// teams-recap-dl downloads a Teams / SharePoint (Stream) meeting recording and
// its transcript. The recording is served as SEA AES-128-CBC encrypted DASH and
// the transcript as encrypted JSON; both use the player's CDN decryption key.
//
// Flow: open the URL in a dedicated Edge profile (SSO happens in the visible
// window), capture the player's manifest / transcript traffic over CDP, then
// download segments with the browser's cookies and the player's live headers,
// decrypt, write .vtt/.txt, and mux the video with ffmpeg.
// Secrets (URLs, tokens, keys) are never printed.
package main

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const version = "1.0.0"

var (
	manifestRE   = regexp.MustCompile(`(?i)/transform/videomanifest\?`)
	segmentRE    = regexp.MustCompile(`(?i)oneDrive\.transcode\?`)
	transcriptRE = regexp.MustCompile(`(?i)/cdnmedia/transcripts\?`)
	errCodeRE    = regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`)
	dropHeaders  = map[string]bool{"cookie": true, "host": true, "content-length": true, "connection": true, "accept-encoding": true}
)

type options struct {
	url, outDir, browser, profile, ffmpeg string
	transcriptOnly, noTranscript          bool
	wait                                  time.Duration
	interactive                           bool
}

func main() {
	opts, err := parseFlags()
	if err == nil {
		err = run(opts)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	}
	if opts.interactive {
		fmt.Print("\nPress Enter to exit...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if err != nil {
		os.Exit(1)
	}
}

func parseFlags() (*options, error) {
	o := &options{}
	flag.StringVar(&o.url, "url", "", "recording URL (Teams recap link or SharePoint stream link)")
	flag.StringVar(&o.outDir, "out", "", "output directory (default: folder picker when interactive, else Downloads)")
	flag.BoolVar(&o.transcriptOnly, "transcript-only", false, "download only the transcript")
	flag.BoolVar(&o.noTranscript, "no-transcript", false, "download only the video")
	flag.StringVar(&o.browser, "browser", "", "path to msedge.exe (default: auto-detect)")
	flag.StringVar(&o.profile, "profile", filepath.Join(os.Getenv("LOCALAPPDATA"), "teams-recap-dl", "profile"), "browser profile directory")
	flag.StringVar(&o.ffmpeg, "ffmpeg", "ffmpeg", "path to ffmpeg")
	waitMin := flag.Int("wait", 10, "minutes to wait for sign-in and the player")
	showVersion := flag.Bool("version", false, "print version")
	flag.BoolVar(&debug, "debug", false, "log captured network events (URLs redacted)")
	flag.Parse()
	if *showVersion {
		fmt.Println("teams-recap-dl", version)
		os.Exit(0)
	}
	o.wait = time.Duration(*waitMin) * time.Minute
	if o.url == "" && flag.NArg() > 0 {
		o.url = flag.Arg(0)
	}
	if o.transcriptOnly && o.noTranscript {
		return o, errors.New("-transcript-only and -no-transcript are mutually exclusive")
	}
	// Double-clicked or run without a URL: prompt for everything.
	if o.url == "" {
		o.interactive = true
		fmt.Print("Paste the recording URL and press Enter:\n> ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		o.url = strings.TrimSpace(line)
		if o.url == "" {
			return o, errors.New("no URL given")
		}
		if o.outDir == "" {
			dir, err := pickFolder()
			if err != nil {
				return o, err
			}
			o.outDir = dir
		}
	}
	if o.outDir == "" {
		home, _ := os.UserHomeDir()
		o.outDir = filepath.Join(home, "Downloads")
	}
	return o, nil
}

// pickFolder shows the Windows folder picker through PowerShell (no extra dependency).
func pickFolder() (string, error) {
	fmt.Println("Choose the download folder in the dialog...")
	script := `Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost=$true}
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = 'Choose where to save the recording and transcript'
$d.SelectedPath = [Environment]::GetFolderPath('UserProfile') + '\Downloads'
if ($d.ShowDialog($owner) -eq 'OK') { $d.SelectedPath }`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script).Output()
	if err != nil {
		return "", fmt.Errorf("folder picker failed: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("no folder selected")
	}
	fmt.Println("Saving to:", dir)
	return dir, nil
}

// normalizeURL turns a Teams recap link into the SharePoint player link it wraps.
func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return "", errors.New("URL must be an https link")
	}
	if f := u.Query().Get("fileUrl"); f != "" {
		return f, nil
	}
	return raw, nil
}

func findEdge() (string, error) {
	for _, p := range []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\Edge\Application\msedge.exe`),
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("Microsoft Edge not found; pass -browser <path to msedge.exe>")
}

// ---------- live capture of the player's traffic ----------

type captured struct {
	url     string
	headers map[string]string
	body    []byte
}

type liveState struct {
	mu         sync.Mutex
	pending    map[network.RequestID]*captured
	kinds      map[network.RequestID]string
	manifest   *captured
	transcript *captured // set on request; body may stay empty (e.g. served from disk cache)
	segHeaders map[string]string
	token      string
}

func (s *liveState) get() (manifest, transcript *captured, segHeaders map[string]string, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manifest, s.transcript, s.segHeaders, s.token
}

func lowerHeaders(h network.Headers) map[string]string {
	m := make(map[string]string, len(h))
	for k, v := range h {
		m[strings.ToLower(k)] = fmt.Sprint(v)
	}
	return m
}

var debug bool

func debugf(format string, a ...any) {
	if debug {
		fmt.Printf("DEBUG "+format+"\n", a...)
	}
}

func listen(ctx context.Context, st *liveState) {
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			u := e.Request.URL
			if strings.Contains(strings.ToLower(u), "transcript") || manifestRE.MatchString(u) {
				debugf("request %s %s %s", e.RequestID, e.Request.Method, redact(u))
			}
			if e.Request.Method == "OPTIONS" {
				return
			}
			h := lowerHeaders(e.Request.Headers)
			st.mu.Lock()
			defer st.mu.Unlock()
			if t := h["x-spopactoken"]; t != "" {
				st.token = t
			}
			switch {
			case segmentRE.MatchString(u):
				st.segHeaders = h
			case manifestRE.MatchString(u) && strings.Contains(strings.ToLower(u), "format=dash") && st.manifest == nil:
				st.pending[e.RequestID] = &captured{url: u, headers: h}
				st.kinds[e.RequestID] = "manifest"
			case transcriptRE.MatchString(u) && st.transcript == nil:
				st.transcript = &captured{url: u, headers: h}
				st.pending[e.RequestID] = st.transcript
				st.kinds[e.RequestID] = "transcript"
			}
		case *network.EventResponseReceived:
			if st.kinds[e.RequestID] != "" {
				debugf("response %s status=%d diskCache=%v sw=%v", e.RequestID, e.Response.Status, e.Response.FromDiskCache, e.Response.FromServiceWorker)
			}
			if e.Response.Status != 200 {
				st.mu.Lock()
				delete(st.pending, e.RequestID)
				st.mu.Unlock()
			}
		case *network.EventLoadingFinished:
			st.mu.Lock()
			c, ok := st.pending[e.RequestID]
			kind := st.kinds[e.RequestID]
			delete(st.pending, e.RequestID)
			st.mu.Unlock()
			if !ok {
				return
			}
			// Fetching the body is a CDP call; it must not block the event loop.
			go func() {
				body, err := network.GetResponseBody(e.RequestID).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target))
				debugf("body %s kind=%s len=%d err=%v", e.RequestID, kind, len(body), err)
				if err != nil || len(body) == 0 {
					return
				}
				st.mu.Lock()
				defer st.mu.Unlock()
				c.body = body
				if kind == "manifest" && st.manifest == nil && strings.Contains(string(body), "<MPD") {
					st.manifest = c
				}
			}()
		}
	})
}

// ---------- HTTP with browser cookies ----------

type fetcher struct {
	ctx     context.Context
	st      *liveState
	client  *http.Client
	mu      sync.Mutex
	cookies map[string]string // host -> Cookie header
}

func (f *fetcher) cookieHeader(rawURL string) string {
	u, _ := url.Parse(rawURL)
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cookies[u.Host]; ok {
		return c
	}
	list, err := network.GetCookies().WithURLs([]string{rawURL}).Do(cdp.WithExecutor(f.ctx, chromedp.FromContext(f.ctx).Target))
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, c := range list {
		parts = append(parts, c.Name+"="+c.Value)
	}
	f.cookies[u.Host] = strings.Join(parts, "; ")
	return f.cookies[u.Host]
}

func (f *fetcher) resetCookies() {
	f.mu.Lock()
	f.cookies = map[string]string{}
	f.mu.Unlock()
}

func (f *fetcher) get(rawURL string, base map[string]string, withToken bool) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range base {
		if !dropHeaders[k] && !strings.HasPrefix(k, ":") && k != "x-spopactoken" {
			req.Header.Set(k, v)
		}
	}
	if _, _, _, token := f.st.get(); withToken && token != "" {
		req.Header.Set("X-Spopactoken", token)
	}
	if c := f.cookieHeader(rawURL); c != "" {
		req.Header.Set("Cookie", c)
	}
	res, err := f.client.Do(req)
	if err != nil {
		return nil, err.Error(), err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err.Error(), err
	}
	if res.StatusCode != http.StatusOK {
		code := errCodeRE.FindSubmatch(body)
		msg := fmt.Sprintf("HTTP %d", res.StatusCode)
		if code != nil {
			msg += " " + string(code[1])
		}
		return nil, msg, errors.New(msg)
	}
	return body, "", nil
}

// fetchSegment mirrors the player: same headers, same token mode first, then the other mode.
func (f *fetcher) fetchSegment(rawURL, label string) ([]byte, error) {
	last := ""
	for attempt := 0; attempt < 6; attempt++ {
		manifest, _, seg, _ := f.st.get()
		base := seg
		if base == nil {
			base = manifest.headers
		}
		playerToken := base["x-spopactoken"] != ""
		for _, withToken := range []bool{playerToken, !playerToken} {
			body, msg, err := f.get(rawURL, base, withToken)
			if err == nil {
				return body, nil
			}
			last = msg
		}
		f.resetCookies()
		time.Sleep(time.Duration(2<<attempt) * time.Second)
	}
	return nil, fmt.Errorf("%s failed: %s", label, last)
}

// ---------- crypto ----------

// decrypt is AES-128-CBC over the whole payload; PKCS7 padding is stripped when valid.
func decrypt(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	if p := int(out[len(out)-1]); p >= 1 && p <= aes.BlockSize {
		valid := true
		for _, b := range out[len(out)-p:] {
			valid = valid && int(b) == p
		}
		if valid {
			out = out[:len(out)-p]
		}
	}
	return out, nil
}

// ---------- DASH manifest ----------

type segTemplate struct {
	Timescale   int64  `xml:"timescale,attr"`
	StartNumber *int64 `xml:"startNumber,attr"`
	Init        string `xml:"initialization,attr"`
	Media       string `xml:"media,attr"`
	S           []struct {
		T *int64 `xml:"t,attr"`
		D int64  `xml:"d,attr"`
		R *int64 `xml:"r,attr"`
	} `xml:"SegmentTimeline>S"`
}

type mpd struct {
	Duration string   `xml:"mediaPresentationDuration,attr"`
	BaseURL  []string `xml:"BaseURL"`
	Periods  []struct {
		BaseURL []string `xml:"BaseURL"`
		Sets    []struct {
			ContentType string   `xml:"contentType,attr"`
			MimeType    string   `xml:"mimeType,attr"`
			BaseURL     []string `xml:"BaseURL"`
			Protection  []struct {
				Crypto []struct {
					KeyURI string `xml:"keyUriTemplate,attr"`
					IV     string `xml:"IV,attr"`
				} `xml:"CryptoPeriod"`
			} `xml:"ContentProtection"`
			Template *segTemplate `xml:"SegmentTemplate"`
			Reps     []struct {
				ID        string       `xml:"id,attr"`
				Bandwidth int64        `xml:"bandwidth,attr"`
				BaseURL   []string     `xml:"BaseURL"`
				Template  *segTemplate `xml:"SegmentTemplate"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

type track struct {
	kind   string
	urls   []string // init segment first
	keyURI string
	iv     []byte
}

var isoDurRE = regexp.MustCompile(`^PT(?:([\d.]+)H)?(?:([\d.]+)M)?(?:([\d.]+)S)?$`)

func isoSeconds(s string) (float64, error) {
	m := isoDurRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("unsupported duration %q", s)
	}
	total := 0.0
	for i, mult := range []float64{3600, 60, 1} {
		if m[i+1] != "" {
			v, _ := strconv.ParseFloat(m[i+1], 64)
			total += v * mult
		}
	}
	return total, nil
}

func expandTimeline(t *segTemplate, end int64) []int64 {
	var times []int64
	var cur int64
	for i, s := range t.S {
		if s.T != nil {
			cur = *s.T
		}
		r := int64(0)
		if s.R != nil {
			r = *s.R
		}
		if r < 0 {
			limit := end
			if i+1 < len(t.S) && t.S[i+1].T != nil {
				limit = *t.S[i+1].T
			}
			r = max(0, (limit-cur+s.D-1)/s.D-1)
		}
		for j := int64(0); j <= r; j++ {
			times = append(times, cur)
			cur += s.D
		}
	}
	return times
}

func expandTemplate(t, repID string, tm, number, bandwidth int64) string {
	return strings.NewReplacer("$RepresentationID$", repID, "$Time$", strconv.FormatInt(tm, 10),
		"$Number$", strconv.FormatInt(number, 10), "$Bandwidth$", strconv.FormatInt(bandwidth, 10)).Replace(t)
}

func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

func buildTracks(body []byte, manifestURL string) ([]track, float64, error) {
	var m mpd
	if err := xml.Unmarshal(body, &m); err != nil {
		return nil, 0, fmt.Errorf("manifest parse: %w", err)
	}
	dur, err := isoSeconds(m.Duration)
	if err != nil {
		return nil, 0, err
	}
	if len(m.Periods) == 0 {
		return nil, 0, errors.New("manifest has no Period")
	}
	p := m.Periods[0]
	var tracks []track
	for _, as := range p.Sets {
		kind := as.ContentType
		if kind == "" {
			kind, _, _ = strings.Cut(as.MimeType, "/")
		}
		if (kind != "video" && kind != "audio") || len(as.Reps) == 0 {
			continue
		}
		rep := as.Reps[0]
		for _, r := range as.Reps {
			if r.Bandwidth > rep.Bandwidth {
				rep = r
			}
		}
		tpl := rep.Template
		if tpl == nil {
			tpl = as.Template
		}
		if tpl == nil {
			return nil, 0, fmt.Errorf("%s track has no SegmentTemplate", kind)
		}
		if tpl.Timescale == 0 {
			tpl.Timescale = 1
		}
		start := int64(1)
		if tpl.StartNumber != nil {
			start = *tpl.StartNumber
		}
		base := manifestURL
		for _, b := range append(append(append(append([]string{}, m.BaseURL...), p.BaseURL...), as.BaseURL...), rep.BaseURL...) {
			if base, err = resolve(base, b); err != nil {
				return nil, 0, err
			}
		}
		t := track{kind: kind}
		for _, cp := range as.Protection {
			for _, c := range cp.Crypto {
				t.keyURI = c.KeyURI
				if c.IV != "" {
					t.iv, _ = hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(c.IV, "0x"), "0X"))
				}
			}
		}
		u, err := resolve(base, expandTemplate(tpl.Init, rep.ID, 0, 0, rep.Bandwidth))
		if err != nil {
			return nil, 0, err
		}
		t.urls = append(t.urls, u)
		for i, tm := range expandTimeline(tpl, int64(dur*float64(tpl.Timescale)+0.999)) {
			if u, err = resolve(base, expandTemplate(tpl.Media, rep.ID, tm, start+int64(i), rep.Bandwidth)); err != nil {
				return nil, 0, err
			}
			t.urls = append(t.urls, u)
		}
		tracks = append(tracks, t)
	}
	return tracks, dur, nil
}

// ---------- transcript ----------

type transcriptDoc struct {
	Entries []struct {
		Text               string `json:"text"`
		SpeakerDisplayName string `json:"speakerDisplayName"`
		SpeakerID          string `json:"speakerId"`
		StartOffset        string `json:"startOffset"`
		EndOffset          string `json:"endOffset"`
	} `json:"entries"`
}

var offsetRE = regexp.MustCompile(`^(\d+):(\d{2}):(\d{2}(?:\.\d+)?)$`)

func parseOffset(s string) time.Duration {
	m := offsetRE.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	sec, _ := strconv.ParseFloat(m[3], 64)
	return time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute + time.Duration(sec*float64(time.Second))
}

func clock(d time.Duration, millis bool) string {
	ms := d.Milliseconds()
	s := fmt.Sprintf("%02d:%02d:%02d", ms/3600000, ms/60000%60, ms/1000%60)
	if millis {
		s += fmt.Sprintf(".%03d", ms%1000)
	}
	return s
}

// renderTranscript returns WebVTT and a plain-text version of the transcript JSON.
func renderTranscript(raw []byte) (vtt, txt string, n int, err error) {
	var doc transcriptDoc
	if err = json.Unmarshal(raw, &doc); err != nil {
		return "", "", 0, fmt.Errorf("transcript parse: %w", err)
	}
	var v, t strings.Builder
	v.WriteString("WEBVTT\n")
	for _, e := range doc.Entries {
		who := e.SpeakerDisplayName
		if who == "" {
			who = e.SpeakerID
		}
		start, end := parseOffset(e.StartOffset), parseOffset(e.EndOffset)
		text := strings.TrimSpace(e.Text)
		fmt.Fprintf(&v, "\n%s --> %s\n<v %s>%s\n", clock(start, true), clock(end, true), who, text)
		fmt.Fprintf(&t, "[%s] %s: %s\n", clock(start, false), who, text)
	}
	return v.String(), t.String(), len(doc.Entries), nil
}

// ---------- main flow ----------

var badName = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]+`)

func baseName(title string) string {
	title = strings.TrimSuffix(strings.TrimSpace(title), ".mp4")
	title = strings.TrimSpace(badName.ReplaceAllString(title, "_"))
	if title == "" {
		return "teams-recap-" + time.Now().Format("20060102-150405")
	}
	return title
}

// newFolder creates parent/name, appending " (n)" if it already exists.
func newFolder(parent, name string) (string, error) {
	for i := 0; ; i++ {
		dir := filepath.Join(parent, name)
		if i > 0 {
			dir = filepath.Join(parent, fmt.Sprintf("%s (%d)", name, i))
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
}

func run(o *options) (err error) {
	target, err := normalizeURL(o.url)
	if err != nil {
		return err
	}
	if !o.transcriptOnly {
		if _, err := exec.LookPath(o.ffmpeg); err != nil {
			return fmt.Errorf("ffmpeg not found (%s); install it or pass -ffmpeg <path>", o.ffmpeg)
		}
	}
	if o.browser == "" {
		if o.browser, err = findEdge(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		return err
	}

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(o.browser),
		chromedp.UserDataDir(o.profile),
		chromedp.Flag("headless", false),
		chromedp.Flag("mute-audio", true),
		// A narrow window collapses the transcript pane, and the player then never loads it.
		chromedp.WindowSize(1600, 1000),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	st := &liveState{pending: map[network.RequestID]*captured{}, kinds: map[network.RequestID]string{}}
	listen(ctx, st)
	fmt.Println("Opening the recording. Complete SSO / MFA in the Edge window if prompted.")
	// Cache off: cached player responses never reach the Network events we capture.
	if err := chromedp.Run(ctx, network.Enable(), network.SetCacheDisabled(true), chromedp.Navigate(target)); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}

	// Wait for what we need, nudging playback so the player requests the manifest.
	deadline, hintAt, hinted := time.Now().Add(o.wait), time.Now().Add(time.Minute), false
	var manifestSeen time.Time
	for {
		manifest, transcript, _, _ := st.get()
		haveVideo := manifest != nil || o.transcriptOnly
		haveTranscript := transcript != nil || o.noTranscript
		if haveVideo && haveTranscript {
			break
		}
		// The transcript is optional: once the manifest is in, give it a short grace period.
		if manifest != nil && manifestSeen.IsZero() {
			manifestSeen = time.Now()
		}
		if manifest != nil && !haveTranscript && time.Since(manifestSeen) > 10*time.Second {
			var clicked bool
			chromedp.Run(ctx, chromedp.Evaluate(openTranscriptJS, &clicked))
			debugf("transcript pane click: %v", clicked)
		}
		if manifest != nil && !haveTranscript && time.Since(manifestSeen) > 30*time.Second {
			if o.transcriptOnly {
				return errors.New("no transcript found for this recording")
			}
			fmt.Println("WARN: no transcript found for this recording; continuing with video only.")
			o.noTranscript = true
			break
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the player; did the recording page load and play?")
		}
		if !hinted && time.Now().After(hintAt) {
			fmt.Println("Still waiting for the player. If the recording does not start, click play in the Edge window.")
			hinted = true
		}
		chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('video').forEach(v => { v.muted = true; v.play().catch(() => {}); })`, nil))
		time.Sleep(3 * time.Second)
	}

	var title string
	chromedp.Run(ctx, chromedp.Title(&title))
	// One folder per download keeps the video and transcript together.
	name := baseName(title)
	folder, err := newFolder(o.outDir, name)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(folder) // only succeeds when nothing was saved
		}
	}()
	outBase := filepath.Join(folder, name)

	f := &fetcher{ctx: ctx, st: st, client: &http.Client{Timeout: 90 * time.Second}, cookies: map[string]string{}}
	key, bootIV := playerKey(ctx)

	manifest, transcript, _, _ := st.get()
	var tracks []track
	var duration float64
	if manifest != nil {
		fmt.Printf("Manifest captured from %s\n", redact(manifest.url))
		if tracks, duration, err = buildTracks(manifest.body, manifest.url); err != nil {
			return err
		}
	}
	if key == nil && len(tracks) > 0 && tracks[0].keyURI != "" {
		if b, _, err := f.get(tracks[0].keyURI, manifest.headers, true); err == nil && len(b) == 16 {
			key = b
		}
	}
	if key == nil {
		return errors.New("could not obtain the decryption key from the player")
	}

	if !o.noTranscript && transcript != nil {
		iv := bootIV
		if iv == nil && len(tracks) > 0 {
			iv = tracks[0].iv
		}
		err := saveTranscript(transcriptBody(f, transcript), key, iv, outBase)
		if err != nil && o.transcriptOnly {
			return err
		}
		if err != nil {
			fmt.Println("WARN:", err)
		}
	}
	if o.transcriptOnly {
		return nil
	}
	if manifest == nil {
		return errors.New("no video manifest captured")
	}

	// Leave a short window for the player to issue real segment requests to mirror.
	for i := 0; i < 20; i++ {
		if _, _, seg, _ := st.get(); seg != nil {
			break
		}
		time.Sleep(time.Second)
	}
	work, err := os.MkdirTemp("", "teams-recap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var inputs []string
	for _, t := range tracks {
		iv := t.iv
		if iv == nil {
			iv = bootIV
		}
		path := filepath.Join(work, t.kind+".mp4")
		if err := downloadTrack(f, t, key, iv, path); err != nil {
			return err
		}
		inputs = append(inputs, "-i", path)
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, inputs...)
	for i := range tracks {
		args = append(args, "-map", strconv.Itoa(i))
	}
	args = append(args, "-c", "copy", "-movflags", "+faststart", outBase+".mp4")
	cmd := exec.Command(o.ffmpeg, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg mux failed: %w", err)
	}
	fmt.Printf("Saved video: %s (%.0fs)\n", outBase+".mp4", duration)
	return nil
}

// openTranscriptJS expands the player's transcript pane, which makes it fetch the transcript.
const openTranscriptJS = `(() => {
	const b = [...document.querySelectorAll('button,[role=tab]')].find(e =>
		/^(read )?transcript$/i.test((e.getAttribute('aria-label') || e.innerText || '').trim()) &&
		e.getAttribute('aria-expanded') !== 'true');
	if (b) b.click();
	return !!b;
})()`

// playerKey reads the CDN decryption key the SharePoint player bootstraps with.
func playerKey(ctx context.Context) (key, iv []byte) {
	var r struct{ Key, IV string }
	err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const d = window.g_streamBootstrapContent?.dashConfig?.cdnDecryptionKey;
		if (!d?.valid) return {Key: '', IV: ''};
		const h = b => Array.from({length: 16}, (_, i) => Number(b[i]).toString(16).padStart(2, '0')).join('');
		return {Key: h(d.keyBuffer), IV: h(d.iv)};
	})()`, &r))
	if err != nil || r.Key == "" {
		return nil, nil
	}
	key, _ = hex.DecodeString(r.Key)
	iv, _ = hex.DecodeString(r.IV)
	return key, iv
}

// transcriptBody returns the player's transcript response, re-fetching its
// signed URL when the browser could not hand over the body.
func transcriptBody(f *fetcher, c *captured) []byte {
	for i := 0; i < 10; i++ {
		f.st.mu.Lock()
		body := c.body
		f.st.mu.Unlock()
		if len(body) > 0 {
			return body
		}
		time.Sleep(500 * time.Millisecond)
	}
	debugf("transcript body not captured; re-fetching")
	for _, withToken := range []bool{false, true} {
		if body, msg, err := f.get(c.url, c.headers, withToken); err == nil {
			return body
		} else {
			debugf("transcript re-fetch token=%v: %s", withToken, msg)
		}
	}
	return nil
}

func saveTranscript(enc, key, iv []byte, outBase string) error {
	if len(enc) == 0 {
		return errors.New("transcript: could not download the transcript")
	}
	if iv == nil {
		return errors.New("transcript: no IV available")
	}
	plain, err := decrypt(enc, key, iv)
	if err != nil {
		return fmt.Errorf("transcript decrypt: %w", err)
	}
	vtt, txt, n, err := renderTranscript(plain)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outBase+".vtt", []byte(vtt), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(outBase+".txt", []byte(txt), 0o644); err != nil {
		return err
	}
	fmt.Printf("Saved transcript: %s.vtt / .txt (%d entries)\n", outBase, n)
	return nil
}

func downloadTrack(f *fetcher, t track, key, iv []byte, path string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	total := len(t.urls) - 1
	// ponytail: serial download; add bounded parallelism if long recordings are too slow.
	for i, u := range t.urls {
		label := fmt.Sprintf("%s segment %d", t.kind, i)
		if i == 0 {
			label = t.kind + " init"
		}
		enc, err := f.fetchSegment(u, label)
		if err != nil {
			return err
		}
		plain, err := decrypt(enc, key, iv)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if i == 0 && (len(plain) < 8 || string(plain[4:8]) != "ftyp") {
			return fmt.Errorf("%s did not decrypt to fMP4 (wrong key/IV?)", label)
		}
		if _, err := out.Write(plain); err != nil {
			return err
		}
		if i%10 == 0 || i == total {
			fmt.Printf("\r%s: %d/%d (%d%%)   ", t.kind, i, total, i*100/max(total, 1))
		}
	}
	fmt.Println()
	return nil
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<url>"
	}
	return u.Host + u.Path
}
