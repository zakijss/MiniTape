package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zakijss/minideck/cache"
)

// A station as the directory lists it, with fields MiniTape doesn't read.
const testStationsJSON = `[
 {"changeuuid":"x","stationuuid":"u1","name":" Night FM ","url":"http://n/pls","url_resolved":"https://n.example/live","favicon":"","tags":"jazz,smooth jazz,lounge,chill","country":"Germany","countrycode":"DE","language":"german,english","codec":"MP3","bitrate":128,"votes":50,"clicktrend":3,"lastcheckok":1},
 {"stationuuid":"u2","name":"Coast AAC","url_resolved":"http://c.example/aac","country":"Portugal","language":"portuguese","codec":"AAC+","bitrate":48,"votes":90,"clicktrend":9,"lastcheckok":1},
 {"stationuuid":"u3","name":"Broken","url_resolved":"http://b.example/","codec":"MP3","bitrate":128,"votes":70,"lastcheckok":0},
 {"stationuuid":"u4","name":"Video","url_resolved":"http://v.example/","codec":"AAC,H.264","bitrate":800,"votes":10,"lastcheckok":1},
 {"stationuuid":"u5","name":"alpha Ogg","url_resolved":"http://o.example/","codec":"OGG","bitrate":256,"votes":5,"clicktrend":20,"lastcheckok":1},
 {"stationuuid":"u6","name":"No address","url_resolved":"file:///etc/passwd","codec":"MP3","bitrate":128,"votes":99,"lastcheckok":1}
]`

func testStations(t *testing.T) []Station {
	t.Helper()
	var st []Station
	if err := json.Unmarshal([]byte(testStationsJSON), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func names(st []Station) string {
	var n []string
	for _, s := range st {
		n = append(n, strings.TrimSpace(s.Name))
	}
	return strings.Join(n, ",")
}

// A station becomes a song with its stream, codec and where it is from;
// only MiniTape's own JSON carries the stream, not a server's.
func TestStationSong(t *testing.T) {
	s := testStations(t)[0].song()
	if s.ID != "radio:u1" || s.Title != "Night FM" || s.Artist != "Germany · German" || s.Genre != "jazz, smooth jazz, lounge" ||
		s.Suffix != "mp3" || s.BitRate != 128 || s.Radio != "https://n.example/live" {
		t.Fatalf("song: %+v", s)
	}
	if got := stationSub(s); got != "Germany · German · MP3 128 kbps" {
		t.Fatalf("sub: %q", got)
	}
	back := fromOwn(ownSongs([]Song{s}))
	if back[0].Radio != s.Radio {
		t.Fatalf("own JSON lost the stream: %+v", back[0])
	}
	var fromServer Song
	json.Unmarshal([]byte(`{"id":"x","Radio":"http://evil/","radioUrl":"http://evil/"}`), &fromServer)
	if fromServer.Radio != "" {
		t.Fatal("a server's reply set a stream address")
	}
	// starred from Now Playing: the song is all there is
	st := stationOf(s)
	if st.id() != s.ID || st.song().Artist != s.Artist || st.song().Radio != s.Radio {
		t.Fatalf("station from the song: %+v", st)
	}
	if srv := stationOf(Station{Server: "7", Name: "Mine", URL: "http://m/"}.song()); srv.Server != "7" || srv.UUID != "" {
		t.Fatalf("server station: %+v", srv)
	}
}

// The options filter and sort every list.
func TestFilterAndSortStations(t *testing.T) {
	for _, c := range []struct {
		cfg  Settings
		want string
	}{
		{Settings{}, "Night FM,Coast AAC,alpha Ogg"}, // as listed
		{Settings{RadioBroken: true}, "Night FM,Coast AAC,Broken,alpha Ogg"},
		{Settings{RadioMP3AAC: true}, "Night FM,Coast AAC"},
		{Settings{RadioSort: "votes"}, "Coast AAC,Night FM,alpha Ogg"},
		{Settings{RadioMinKbps: 64}, "Night FM,alpha Ogg"},
		{Settings{RadioSort: "trend"}, "alpha Ogg,Coast AAC,Night FM"},
		{Settings{RadioSort: "name"}, "alpha Ogg,Coast AAC,Night FM"},
		{Settings{RadioSort: "bitrate"}, "alpha Ogg,Night FM,Coast AAC"},
		{Settings{RadioSort: "nonsense"}, "Night FM,Coast AAC,alpha Ogg"},
	} {
		st := filterStations(testStations(t), c.cfg)
		sortStations(st, c.cfg.radioSort())
		if got := names(st); got != c.want {
			t.Errorf("%+v: %s, want %s", c.cfg, got, c.want)
		}
	}
	// the server's stations say nothing of codecs and checks: only the
	// address counts
	srv := []Station{{Server: "1", Name: "a", URL: "http://a/"}, {Server: "2", Name: "b", URL: "rtsp://b/"}}
	if got := names(filterStations(srv, Settings{RadioMP3AAC: true, RadioMinKbps: 128})); got != "a" {
		t.Fatalf("server stations: %s", got)
	}
}

// The options never hide a starred station; other lists are filtered.
func TestFavouritesUnfiltered(t *testing.T) {
	a := testApp(t, "")
	a.cfg = &Settings{RadioMP3AAC: true, RadioMinKbps: 128, RadioSort: "name"}
	all := func() ([]Station, error) { return testStations(t), nil }
	fav := a.stationList("Favourites", "", false, all)
	a.push(fav)
	a.waitLoaded(t, fav)
	other := a.stationList("Popular", "", true, all)
	a.push(other)
	a.waitLoaded(t, other)
	var got []string
	for _, it := range fav.list.Items {
		got = append(got, it.Label)
	}
	if strings.Join(got, ",") != "alpha Ogg,Broken,Coast AAC,Night FM,No address,Video" || len(other.list.Items) != 1 {
		t.Fatalf("favourites %q, other list %d stations", got, len(other.list.Items))
	}
}

// Left and right step through the options, A too.
func TestStepRadio(t *testing.T) {
	var cfg Settings
	if cfg.radioSort() != "list" {
		t.Fatalf("default sort %q", cfg.radioSort())
	}
	for _, want := range []string{"votes", "trend", "name", "bitrate", "list"} {
		stepRadio(&cfg, "Sort by", 1)
		if cfg.radioSort() != want {
			t.Fatalf("sort %q, want %q", cfg.RadioSort, want)
		}
	}
	stepRadio(&cfg, "Sort by", -1)
	stepRadio(&cfg, "Lowest bit rate", 1)
	stepRadio(&cfg, "Broken stations", 1)
	stepRadio(&cfg, "MP3 and AAC only", 1)
	if cfg.radioSort() != "bitrate" || cfg.RadioMinKbps != 64 || !cfg.RadioBroken || !cfg.RadioMP3AAC {
		t.Fatalf("options: %+v", cfg)
	}
	if stepRadio(&cfg, "Search", 1) {
		t.Fatal("a row without a value stepped")
	}
	if got := radioOptionItems(cfg)[3].Detail; got != "64 kbit/s" {
		t.Fatalf("bit rate row: %q", got)
	}
}

// The directory: its servers come from the lookup, one that doesn't answer
// is passed over, every request names MiniTape, and lists are kept on the
// card for their lifetime and shown from there when nothing answers.
func TestRadioDirectory(t *testing.T) {
	var hits atomic.Int32
	var down atomic.Bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "MiniTape/") {
			t.Errorf("user agent %q", r.UserAgent())
		}
		switch r.URL.Path {
		case "/json/servers":
			// a dead one, then this one
			fmt.Fprintf(w, `[{"name":"http://127.0.0.1:1"},{"name":%q},{"name":""}]`, srv.URL)
			return
		}
		hits.Add(1)
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		switch r.URL.Path {
		case "/json/stations/search":
			if q.Get("hidebroken") != "true" || q.Get("limit") != "100" || q.Get("reverse") != "true" ||
				q.Get("order") != "votes" || q.Get("tag") != "jazz" || q.Get("tagExact") != "true" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			io.WriteString(w, testStationsJSON)
		case "/json/countries":
			io.WriteString(w, `[{"name":"Germany","iso_3166_1":"DE","stationcount":3000},{"name":"","iso_3166_1":"XX","stationcount":1},{"name":"Nowhere","iso_3166_1":"","stationcount":2}]`)
		case "/json/url/u1":
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := &radioDir{http: srv.Client(), cache: cache.New(t.TempDir()), lookup: srv.URL + "/json/servers"}
	q := groupQuery("tags", radioGroup{Name: "jazz"})
	st, err := d.stations(q, true, false)
	if err != nil || len(st) != 6 || st[0].UUID != "u1" || st[0].Votes != 50 || st[1].Trend != 9 {
		t.Fatalf("stations: %v, %+v", err, st)
	}
	if q.Has("limit") {
		t.Fatal("the caller's query was changed")
	}
	if d.hosts[0] != srv.URL {
		t.Fatalf("the server that answered isn't first: %v", d.hosts)
	}
	// kept: the second time doesn't ask
	if st, err := d.stations(q, true, false); err != nil || len(st) != 6 || hits.Load() != 1 {
		t.Fatalf("from the card: %v, %d stations, %d requests", err, len(st), hits.Load())
	}
	// Y asks anyway; with the directory down, the copy kept is shown
	down.Store(true)
	if st, err := d.stations(q, true, true); err != nil || len(st) != 6 || hits.Load() != 2 {
		t.Fatalf("offline: %v, %d stations, %d requests", err, len(st), hits.Load())
	}
	// a search isn't kept, so offline it fails
	if _, err := d.stations(map[string][]string{"name": {"night"}}, true, false); err == nil {
		t.Fatal("a search with the directory down")
	}
	down.Store(false)
	gs, err := d.groups("countries", false)
	if err != nil || len(gs) != 1 || gs[0].Code != "DE" || gs[0].Count != 3000 {
		t.Fatalf("countries: %v, %+v", err, gs)
	}
	if q := groupQuery("countries", gs[0]); q.Get("countrycode") != "DE" {
		t.Fatalf("country query: %v", q)
	}
	before := hits.Load()
	d.click("u1")
	if hits.Load() != before+1 {
		t.Fatal("the listen wasn't counted")
	}
}

// Without the lookup, the known servers are asked.
func TestRadioServersFallback(t *testing.T) {
	d := &radioDir{http: &http.Client{Timeout: time.Second}, lookup: "http://127.0.0.1:1/json/servers"}
	if got := d.servers(); len(got) != len(radioHosts) || got[0] != radioHosts[0] {
		t.Fatalf("servers: %v", got)
	}
	if d.hosts != nil {
		t.Fatal("the fallback was kept instead of asking again next time")
	}
}

// The favourites are kept in the app's folder.
func TestFavourites(t *testing.T) {
	path := filepath.Join(t.TempDir(), radioFavouritesFile)
	f := loadFavourites(path)
	st := testStations(t)
	if f.has("radio:u1") || len(f.all()) != 0 {
		t.Fatal("favourites before any")
	}
	for _, s := range []Station{st[0], st[1], st[0]} { // on, on, off
		if err := f.toggle(s); err != nil {
			t.Fatal(err)
		}
	}
	g := loadFavourites(path)
	if g.has("radio:u1") || !g.has("radio:u2") || len(g.all()) != 1 || g.all()[0].Codec != "AAC+" {
		t.Fatalf("read back: %+v", g.all())
	}
	var none *favourites
	if none.has("radio:u2") {
		t.Fatal("nil favourites")
	}
}

// icyStream is audio as a station with metaint every sends it: after each
// every bytes, a metadata block (the next title, or none).
func icyStream(audio []byte, every int, titles ...string) []byte {
	var b bytes.Buffer
	for len(audio) > 0 {
		n := min(every, len(audio))
		b.Write(audio[:n])
		audio = audio[n:]
		if n < every {
			break
		}
		if len(titles) == 0 {
			b.WriteByte(0)
			continue
		}
		meta := []byte("StreamTitle='" + titles[0] + "';StreamUrl='';")
		titles = titles[1:]
		k := (len(meta) + 15) / 16
		b.WriteByte(byte(k))
		b.Write(append(meta, make([]byte, k*16-len(meta))...))
	}
	return b.Bytes()
}

// The metadata comes out of the audio, and the titles out of it.
func TestICYReader(t *testing.T) {
	audio := bytes.Repeat([]byte("0123456789"), 50)
	var titles []string
	r := &icyReader{r: bytes.NewReader(icyStream(audio, 64, "A - One", "", "B - Two")), every: 64, left: 64,
		title: func(s string) { titles = append(titles, s) }}
	got, err := io.ReadAll(iotest(r))
	if err != nil || !bytes.Equal(got, audio) {
		t.Fatalf("audio: %v, %d bytes", err, len(got))
	}
	if strings.Join(titles, "|") != "A - One||B - Two" {
		t.Fatalf("titles: %q", titles)
	}
	for meta, want := range map[string]string{
		"StreamTitle='Guns N' Roses - Don't Cry';StreamUrl='x';": "Guns N' Roses - Don't Cry",
		"StreamTitle='Caf\xe9 del Mar';\x00\x00":                 "Café del Mar",
		"StreamTitle='Björk - Jóga'\x00\x00":                     "Björk - Jóga",
		"StreamTitle='';":                                        "",
	} {
		if got, ok := streamTitle(meta); !ok || got != want {
			t.Errorf("%q: %q", meta, got)
		}
	}
	if _, ok := streamTitle("StreamUrl='x';"); ok {
		t.Error("a title in metadata without one")
	}
}

// iotest reads r a few bytes at a time, as a slow link would.
func iotest(r io.Reader) io.Reader {
	return readerFunc(func(p []byte) (int, error) { return r.Read(p[:min(len(p), 7)]) })
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// An old SHOUTcast server's "ICY 200 OK" reads as HTTP.
func TestICYStatusLine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		io.WriteString(c, "ICY 200 OK\r\nicy-name: Old\r\ncontent-type: audio/mpeg\r\n\r\naudio")
	}()
	resp, err := radioClient().Get("http://" + ln.Addr().String() + "/;")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Icy-Name") != "Old" || string(b) != "audio" {
		t.Fatalf("HTTP %d, %q, %q", resp.StatusCode, resp.Header, b)
	}
}

// The relay passes a station's audio to ffplay without the metadata, keeps
// its song title, and connects again when the stream drops.
func TestRelayRadio(t *testing.T) {
	first := bytes.Repeat([]byte("a"), 80<<10) // over the cushion, so it starts at once
	second := bytes.Repeat([]byte("b"), 3000)
	var conns atomic.Int32
	station := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Icy-MetaData") != "1" {
			t.Errorf("no Icy-MetaData")
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Icy-Metaint", "1000")
		if conns.Add(1) == 1 {
			w.Write(icyStream(first, 1000, "First - Song"))
			return // the stream drops
		}
		w.Write(icyStream(second, 1000, "Second - Song"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer station.Close()
	rl, err := startRelay(func(string, int, int) string { return "" }, func() int { return 192 },
		func() string { return "mp3" }, "", func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer rl.close()
	s := Station{UUID: "u1", Name: "Night FM", URL: station.URL + "/live", Codec: "MP3"}.song()
	resp, err := http.Get(rl.URL(s, 30)) // a station ignores the time asked for
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("HTTP %d %q", resp.StatusCode, resp.Header)
	}
	got := make([]byte, len(first)+len(second))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(append([]byte(nil), first...), second...)) {
		t.Fatal("the audio isn't the station's without its metadata")
	}
	if conns.Load() != 2 {
		t.Fatalf("%d connections", conns.Load())
	}
	if got := rl.liveTitle(s.ID); got != "Second - Song" {
		t.Fatalf("live title %q", got)
	}
	if rl.liveTitle("radio:other") != "" {
		t.Fatal("another station's title")
	}
	// a stream address that isn't the web is refused
	bad := Song{ID: "radio:bad", Radio: "file:///etc/passwd"}
	if resp, err := http.Get(rl.URL(bad, 0)); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("file stream: %v %v", err, resp.StatusCode)
	}
}

// A station plays live: no seeking, A stops it and plays it again from
// now, L1 goes straight to the previous station, and a stream that ends is
// tuned in again as the same play.
func TestPlayerRadio(t *testing.T) {
	fakeFFplay(t, "exec sleep 30")
	var urls atomic.Int32
	p := NewPlayer(func(s Song, at int) string {
		urls.Add(1)
		if at != 0 {
			t.Errorf("station asked for at %d s", at)
		}
		return "http://relay/" + s.ID
	}, nil)
	p.prefetch = func(s Song) { t.Errorf("prefetched %s", s.ID) }
	defer p.Stop()
	q := []Song{{ID: "radio:a", Radio: "http://a/"}, {ID: "radio:b", Radio: "http://b/"}}
	p.Play(q, 0)
	p.Seek(30)
	if urls.Load() != 1 {
		t.Fatal("seeking restarted a station")
	}
	p.Toggle()
	if st := p.State(); st.Playing || st.Paused {
		t.Fatalf("paused station: %+v", st)
	}
	p.Toggle()
	if st := p.State(); !st.Playing || urls.Load() != 2 {
		t.Fatalf("played again: %+v", st)
	}
	p.Next()
	p.mu.Lock()
	p.started = time.Now().Add(-time.Minute) // listened a while
	p.mu.Unlock()
	p.Prev()
	if st := p.State(); st.Pos != 0 {
		t.Fatalf("L1 went to %d", st.Pos)
	}
	p.Restore(q, 1, time.Hour, 0)
	if st := p.State(); st.Elapsed != 0 {
		t.Fatalf("a station restored at %v", st.Elapsed)
	}

	// the stream ends after a while: tuned in again, the same play
	fakeFFplay(t, "sleep 0.3; exit 0")
	ended := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	ended.minPlayed = 100 * time.Millisecond
	defer ended.Stop()
	ended.Play(q, 1)
	plays := ended.State().Plays
	time.Sleep(800 * time.Millisecond)
	if st := ended.State(); st.Pos != 1 || st.Plays != plays || !st.Playing || st.Err != "" {
		t.Fatalf("after the stream ended: %+v", st)
	}

	// ffplay fails on it: an error, not another try
	fakeFFplay(t, "sleep 0.3; exit 1")
	failed := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	failed.minPlayed = 100 * time.Millisecond
	defer failed.Stop()
	failed.Play(q, 0)
	time.Sleep(600 * time.Millisecond)
	if st := failed.State(); st.Playing || st.Err == "" {
		t.Fatalf("after ffplay failed: %+v", st)
	}
}

// A saved station plays on with no server set up.
func TestResumeStation(t *testing.T) {
	dir := t.TempDir()
	q := []Song{{ID: "radio:a", Title: "Night FM", Radio: "http://a/"}, {ID: "s1", Title: "Server song"}}
	writeJSON(filepath.Join(dir, savedQueueFile), savedQueue{"http://old u", ownSongs(q)})
	writeJSON(filepath.Join(dir, savedPlaceFile), savedPlace{Pos: 0, ID: "radio:a", Sec: 600})
	got, pos, _, _ := loadResume(dir, "", "/m")
	if len(got) != 1 || got[0].Radio != "http://a/" || pos != 0 {
		t.Fatalf("resumed %+v at %d", got, pos)
	}
}

// The server's stations come from getInternetRadioStations.
func TestServerStations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"subsonic-response":{"status":"ok","internetRadioStations":{"internetRadioStation":[{"id":"7","name":"Home FM","streamUrl":"http://home:8000/live","homePageUrl":"http://home"}]}}}`)
	}))
	defer srv.Close()
	c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client()}
	st, err := c.InternetRadioStations()
	if err != nil || len(st) != 1 || st[0].song().ID != "radio-server:7" || st[0].URL != "http://home:8000/live" {
		t.Fatalf("%v %+v", err, st)
	}
}
