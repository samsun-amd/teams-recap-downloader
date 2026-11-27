package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestDecryptNISTVector(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	iv, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	ct, _ := hex.DecodeString("7649abac8119b246cee98e9b12e9197d")
	got, err := decrypt(ct, key, iv)
	if err != nil || hex.EncodeToString(got) != "6bc1bee22e409f96e93d7e117393172a" {
		t.Fatalf("decrypt = %x, %v", got, err)
	}
}

func TestBuildTracks(t *testing.T) {
	manifest := `<MPD xmlns="urn:mpeg:DASH:schema:MPD:2011" xmlns:sea="urn:mpeg:dash:schema:sea:2012" mediaPresentationDuration="PT0H0M15S">
  <BaseURL>https://host.invalid/drives/x/items/y/</BaseURL>
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <ContentProtection><sea:CryptoPeriod keyUriTemplate="https://key.invalid/k" IV="0x000102030405060708090A0B0C0D0E0F"/></ContentProtection>
      <SegmentTemplate timescale="16000" initialization="oneDrive.transcode?part=initsegment&amp;q=$RepresentationID$" media="oneDrive.transcode?part=media&amp;q=$RepresentationID$&amp;segmentTime=$Time$">
        <SegmentTimeline><S d="96000" r="1"/><S d="48000"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="low" bandwidth="10"/>
      <Representation id="vcopy" bandwidth="1000"/>
    </AdaptationSet>
  </Period>
</MPD>`
	tracks, dur, err := buildTracks([]byte(manifest), "https://mediap.invalid/transform/videomanifest?a=1")
	if err != nil {
		t.Fatal(err)
	}
	if dur != 15 || len(tracks) != 1 {
		t.Fatalf("dur=%v tracks=%d", dur, len(tracks))
	}
	tr := tracks[0]
	if len(tr.urls) != 4 || tr.keyURI != "https://key.invalid/k" || len(tr.iv) != 16 {
		t.Fatalf("bad track: %d urls, key=%q iv=%x", len(tr.urls), tr.keyURI, tr.iv)
	}
	if tr.urls[0] != "https://host.invalid/drives/x/items/y/oneDrive.transcode?part=initsegment&q=vcopy" {
		t.Fatalf("init url = %s", tr.urls[0])
	}
	if !strings.HasSuffix(tr.urls[3], "q=vcopy&segmentTime=192000") {
		t.Fatalf("last url = %s", tr.urls[3])
	}
}

func TestOpenEndedRepeat(t *testing.T) {
	r := int64(-1)
	tpl := &segTemplate{}
	tpl.S = append(tpl.S, struct {
		T *int64 `xml:"t,attr"`
		D int64  `xml:"d,attr"`
		R *int64 `xml:"r,attr"`
	}{D: 10, R: &r})
	if got := expandTimeline(tpl, 35); len(got) != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestRenderTranscript(t *testing.T) {
	raw := `{"entries":[{"text":" Hello ","speakerDisplayName":"A","startOffset":"00:01:26.2598585","endOffset":"00:01:26.8198585"},
	{"text":"Hi","speakerId":"id2","startOffset":"01:00:00.5","endOffset":"01:00:01"}]}`
	vtt, txt, n, err := renderTranscript([]byte(raw))
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if !strings.Contains(vtt, "00:01:26.259 --> 00:01:26.819\n<v A>Hello\n") || !strings.HasPrefix(vtt, "WEBVTT\n") {
		t.Fatalf("vtt:\n%s", vtt)
	}
	if txt != "[00:01:26] A: Hello\n[01:00:00] id2: Hi\n" {
		t.Fatalf("txt:\n%s", txt)
	}
}

func TestNormalizeURL(t *testing.T) {
	got, err := normalizeURL("https://teams.microsoft.com/l/meetingrecap?driveId=x&fileUrl=https%3A%2F%2Fa.sharepoint.com%2F%3Av%3A%2Fg%2Fp")
	if err != nil || got != "https://a.sharepoint.com/:v:/g/p" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := normalizeURL("http://x"); err == nil {
		t.Fatal("expected error for non-https URL")
	}
}
