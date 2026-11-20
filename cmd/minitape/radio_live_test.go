package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zakijss/minideck/tnet"
)

// TestRadioLive checks the radio against the real directory and real
// stations: the server list, Popular, a genre, and an MP3 and an AAC
// station streamed through the relay for about 10 s each, their audio
// checked frame by frame (the ICY metadata must be gone). It needs the
// network, so it runs only with MINITAPE_RADIO_LIVE=1; with
// MINITAPE_RADIO_OUT=DIR the relayed audio is kept there for ffprobe.
func TestRadioLive(t *testing.T) {
	if os.Getenv("MINITAPE_RADIO_LIVE") != "1" {
		t.Skip("set MINITAPE_RADIO_LIVE=1 to check the radio against the real directory")
	}
	d := &radioDir{http: tnet.Client(20 * time.Second)}
	hosts := d.servers()
	t.Logf("directory servers: %v", hosts)

	var popular []Station
	q := url.Values{"order": {"votes"}, "reverse": {"true"}, "limit": {"10"}, "hidebroken": {"true"}}
	if err := d.get("/json/stations/search", q, 0, true, &popular); err != nil {
		t.Fatalf("Popular: %v", err)
	}
	t.Logf("Popular, %d stations:", len(popular))
	for _, s := range popular[:min(5, len(popular))] {
		t.Logf("  %-32.32s %-6s %3d kbps  %s  votes %d", s.Name, s.Codec, s.Bitrate, s.place(), s.Votes)
	}
	gs, err := d.groups("tags", true)
	if err != nil || len(gs) == 0 {
		t.Fatalf("genres: %v", err)
	}
	genre, err := d.stations(groupQuery("tags", gs[0]), true, true)
	if err != nil {
		t.Fatalf("genre %s: %v", gs[0].Name, err)
	}
	kept := filterStations(genre, Settings{})
	t.Logf("genres: %d, the first %q (%d stations); its list: %d stations, %d playable", len(gs), gs[0].Name, gs[0].Count, len(genre), len(kept))

	rl, err := startRelay(func(string, int, int) string { return "" }, func() int { return 192 },
		func() string { return "mp3" }, "", func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer rl.close()
	for _, codec := range []string{"MP3", "AAC"} {
		cands, err := d.stations(url.Values{"codec": {codec}, "order": {"votes"}}, true, true)
		if err != nil {
			t.Fatalf("%s stations: %v", codec, err)
		}
		cands = filterStations(cands, Settings{})
		played := false
		for _, s := range cands[:min(4, len(cands))] { // the next one if a station doesn't answer
			if played = liveStream(t, rl, s, codec); played {
				break
			}
		}
		if !played {
			t.Errorf("no %s station played", codec)
		}
	}
}

// liveStream reads station s through the relay for about 10 s and checks
// its frames. It reports whether audio came.
func liveStream(t *testing.T, rl *relay, s Station, codec string) bool {
	song := s.song()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", rl.URL(song, 0), nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("%s %q: %v", codec, s.Name, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Logf("%s %q (%s): HTTP %d %s", codec, s.Name, s.URL, resp.StatusCode, b)
		return false
	}
	var first time.Duration
	var audio []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 && first == 0 {
			first = time.Since(start)
		}
		audio = append(audio, buf[:n]...)
		if err != nil {
			break
		}
	}
	secs := time.Since(start).Seconds()
	frames, inFrames, resyncs := walkFrames(audio, codec)
	t.Logf("%s %q (%s, %d kbps listed): %s, first audio after %.1f s, %d bytes in %.1f s (%.0f kbit/s), %d frames covering %.1f%% of the bytes, %d resyncs; title %q",
		codec, s.Name, s.URL, s.Bitrate, resp.Header.Get("Content-Type"), first.Seconds(), len(audio), secs,
		float64(len(audio))*8/1000/secs, frames, 100*float64(inFrames)/float64(max(1, len(audio))), resyncs, rl.liveTitle(song.ID))
	if dir := os.Getenv("MINITAPE_RADIO_OUT"); dir != "" {
		os.WriteFile(filepath.Join(dir, "radio-"+codec+".bin"), audio, 0o644)
	}
	if len(audio) > 0 && frames < 10 {
		t.Errorf("%s: the audio isn't %s frames", s.Name, codec)
	}
	return len(audio) > 0
}

// walkFrames follows MP3 or ADTS AAC frame headers through b: how many
// frames it found, the bytes they cover, and how often it lost the chain
// and had to look for the next header (ICY metadata left in would do that).
func walkFrames(b []byte, codec string) (frames, covered, resyncs int) {
	size := mp3Frame
	if codec == "AAC" {
		size = adtsFrame
	}
	// a frame counts once the next one follows it
	at := func(i int) int {
		n := size(b[i:])
		if n <= 0 || i+n+4 > len(b) || size(b[i+n:]) <= 0 {
			return 0
		}
		return n
	}
	synced := false
	for i := 0; i+4 < len(b); {
		if n := at(i); n > 0 {
			frames, covered, synced = frames+1, covered+n, true
			i += n
			continue
		}
		if synced && len(b)-i > 8<<10 { // not the last frame, cut short
			resyncs++
			synced = false
		}
		i++
	}
	return frames, covered, resyncs
}

// mp3Frame is the length of the MPEG audio frame whose header starts b, or
// 0 if it isn't one.
func mp3Frame(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0
	}
	ver, layer := b[1]>>3&3, b[1]>>1&3 // ver 3: MPEG 1, 2: MPEG 2, 0: MPEG 2.5
	bri, sri, pad := int(b[2]>>4), int(b[2]>>2&3), int(b[2]>>1&1)
	if ver == 1 || layer != 1 || bri == 0 || bri == 15 || sri == 3 { // Layer III only
		return 0
	}
	rates := [][]int{{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}, {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}}
	srs := map[byte][]int{3: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 0: {11025, 12000, 8000}}
	if ver == 3 {
		return 144*rates[1][bri]*1000/srs[ver][sri] + pad
	}
	return 72*rates[0][bri]*1000/srs[ver][sri] + pad
}

// adtsFrame is the length of the ADTS frame whose header starts b, or 0.
func adtsFrame(b []byte) int {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
		return 0
	}
	n := int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5]>>5)
	if n < 7 {
		return 0
	}
	return n
}
