package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// song is stand-in audio: n bytes, different for each id.
func song(id string, n int) []byte {
	r := rand.New(rand.NewPCG(uint64(len(id)), uint64(id[0])))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	return b
}

// fakeServer serves /rest/stream?id=ID; pause, if set, is called after
// each 64 KB written. It counts requests and remembers cancelled ones.
type fakeServer struct {
	*httptest.Server
	mu        sync.Mutex
	requests  map[string]int
	cancelled map[string]bool
	queries   map[string]string
	ranges    map[string][]string // Range headers asked for, by id
	sent      atomic.Int64
	// cut, if set, is how many bytes the server sends of request number n
	// (from 1) for id before it stops (0: all); withLength makes it announce
	// the whole length (a file passed through), so stopping is an error.
	cut          func(id string, n int) int
	withLength   bool
	ignoreOffset bool         // timeOffset is ignored: every stream starts at 0
	delay        atomic.Int64 // how long the server takes to start a stream (ns)
	size         int
	pause        func(id string, sent int)
}

func newFakeServer(t *testing.T, size int) *fakeServer {
	fs := &fakeServer{requests: map[string]int{}, cancelled: map[string]bool{}, queries: map[string]string{}, ranges: map[string][]string{}, size: size}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		time.Sleep(time.Duration(fs.delay.Load()))
		fs.mu.Lock()
		fs.requests[id]++
		nreq := fs.requests[id]
		fs.queries[id] = r.URL.RawQuery
		if rg := r.Header.Get("Range"); rg != "" {
			fs.ranges[id] = append(fs.ranges[id], rg)
		}
		pause, cut, withLength, ignoreOffset := fs.pause, fs.cut, fs.withLength, fs.ignoreOffset
		fs.mu.Unlock()
		if id == "missing" {
			http.Error(w, "no such song", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		data := song(id, fs.size)
		status := http.StatusOK
		if t, _ := strconv.Atoi(r.URL.Query().Get("t")); t > 0 && !ignoreOffset { // timeOffset: 192 kbit/s
			data = data[min(len(data), t*192*125):]
		}
		if rg := r.Header.Get("Range"); rg != "" {
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			data, status = data[min(len(data), from):], http.StatusPartialContent
		}
		if withLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		if cut != nil {
			if n := cut(id, nreq); n > 0 && n < len(data) {
				w.WriteHeader(status)
				w.Write(data[:n])
				if withLength { // stop mid-file: the client sees it end early
					if hj, ok := w.(http.Hijacker); ok {
						w.(http.Flusher).Flush()
						c, _, _ := hj.Hijack()
						c.Close()
					}
				}
				return
			}
		}
		w.WriteHeader(status)
		for i := 0; i < len(data); i += 64 << 10 {
			if _, err := w.Write(data[i:min(len(data), i+64<<10)]); err != nil {
				fs.mu.Lock()
				fs.cancelled[id] = true
				fs.mu.Unlock()
				return
			}
			fs.sent.Add(int64(min(len(data), i+64<<10) - i))
			if pause != nil {
				pause(id, i+64<<10)
			}
		}
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) lastQuery(id string) string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.queries[id]
}

func (fs *fakeServer) setCut(withLength bool, cut func(id string, n int) int) {
	fs.mu.Lock()
	fs.cut, fs.withLength = cut, withLength
	fs.mu.Unlock()
}

func (fs *fakeServer) setPause(p func(id string, sent int)) {
	fs.mu.Lock()
	fs.pause = p
	fs.mu.Unlock()
}

func (fs *fakeServer) count(id string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.requests[id]
}

func newTestRelay(t *testing.T, fs *fakeServer) *relay {
	return newTestRelayIn(t, fs, t.TempDir())
}

// newTestRelayIn keeps songs in dir ("": memory only). The server URL
// records the bit rate asked for.
func newTestRelayIn(t *testing.T, fs *fakeServer, dir string) *relay {
	r, err := startRelay(func(id string, at, kbps int) string {
		return fmt.Sprintf("%s/rest/stream?id=%s&maxBitRate=%d&t=%d", fs.URL, id, kbps, at)
	}, func() int { return 192 }, func() string { return "mp3" }, dir, func() int64 { return 500 << 20 })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close) // before the server's Close, which waits for open streams
	return r
}

func TestRelayPassesTheStreamThrough(t *testing.T) {
	fs := newFakeServer(t, 3<<20+12345)
	r := newTestRelay(t, fs)
	resp, err := http.Get(r.URL(Song{ID: "abc"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "audio/mpeg" || !bytes.Equal(got, song("abc", fs.size)) {
		t.Fatalf("got %d bytes (%s), want the song's %d", len(got), resp.Header.Get("Content-Type"), fs.size)
	}
}

func TestRelayServerError(t *testing.T) {
	fs := newFakeServer(t, 1000)
	r := newTestRelay(t, fs)
	resp, err := http.Get(r.URL(Song{ID: "missing"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), "no such song") {
		t.Fatalf("HTTP %d %q", resp.StatusCode, b)
	}
}

// While ffplay reads slowly, the relay downloads the whole song as fast
// as the server sends it.
func TestRelayDownloadsAhead(t *testing.T) {
	fs := newFakeServer(t, 6<<20)
	r := newTestRelay(t, fs)
	resp, err := http.Get(r.URL(Song{ID: "abc"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadFull(resp.Body, make([]byte, 10<<10))
	deadline := time.Now().Add(5 * time.Second)
	for fs.sent.Load() < int64(fs.size) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.sent.Load() < int64(fs.size) {
		t.Fatalf("downloaded only %d of %d bytes while ffplay read 10 KB", fs.sent.Load(), fs.size)
	}
}

// A song played once plays again from the card, without the server; a
// song skipped half way leaves nothing behind.
func TestRelayKeepsSongsOnTheCard(t *testing.T) {
	fs := newFakeServer(t, 700<<10)
	dir := t.TempDir()
	r := newTestRelayIn(t, fs, dir)
	get := func(id string, n int) []byte {
		resp, err := http.Get(r.URL(Song{ID: id}, 0))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if n < 0 {
			b, _ := io.ReadAll(resp.Body)
			return b
		}
		b := make([]byte, n)
		io.ReadFull(resp.Body, b)
		return b
	}
	get("a", -1)
	time.Sleep(100 * time.Millisecond) // kept once the download ends
	if b := get("a", -1); !bytes.Equal(b, song("a", fs.size)) {
		t.Fatalf("second play: %d bytes", len(b))
	}
	if n := fs.count("a"); n != 1 {
		t.Errorf("the server was asked %d times for a song already on the card", n)
	}
	// skipped: a slow server, and ffplay leaves after 64 KB
	fs.setPause(func(string, int) { time.Sleep(20 * time.Millisecond) })
	get("b", 64<<10)
	time.Sleep(300 * time.Millisecond)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".part-") {
			t.Errorf("a skipped download was left on the card: %s", e.Name())
		}
	}
	if len(ents) != 1 {
		t.Errorf("%d files on the card, want 1 (song a)", len(ents))
	}
}

// Without a card to keep songs on, they stream through memory.
func TestRelayWithoutCard(t *testing.T) {
	fs := newFakeServer(t, 900<<10)
	r := newTestRelayIn(t, fs, "")
	resp, err := http.Get(r.URL(Song{ID: "abc"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(b, song("abc", fs.size)) {
		t.Fatalf("got %d bytes", len(b))
	}
}

// When the music waits for the download, the next songs are asked for a
// step lower; after quick downloads it goes back up.
func TestRelayQuality(t *testing.T) {
	fs := newFakeServer(t, 400<<10)
	r := newTestRelay(t, fs)
	rate := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.rate()
	}
	var mu sync.Mutex
	slowID := "slow1"
	fs.setPause(func(id string, sent int) {
		mu.Lock()
		slow := id == slowID
		mu.Unlock()
		if slow && sent == 320<<10 { // after the prebuffer
			time.Sleep(1300 * time.Millisecond) // the link drops mid-song
		}
	})
	play := func(id string) {
		resp, err := http.Get(r.URL(Song{ID: id}, 0))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	play("slow1")
	if got := rate(); got != 128 {
		t.Fatalf("after the music waited: %d kbit/s, want 128", got)
	}
	mu.Lock()
	slowID = "slow2"
	mu.Unlock()
	play("slow2")
	if got := rate(); got != 96 {
		t.Fatalf("after waiting again: %d kbit/s, want 96", got)
	}
	play("slow3") // no lower step: stays
	if got := rate(); got != 96 {
		t.Fatalf("%d kbit/s, want 96", got)
	}
	if !strings.Contains(fs.lastQuery("slow3"), "maxBitRate=96") {
		t.Errorf("asked the server for %q", fs.lastQuery("slow3"))
	}
	for _, id := range []string{"q1", "q2", "q3", "q4"} {
		play(id)
	}
	if got := rate(); got != 192 {
		t.Errorf("after four quick songs: %d kbit/s, want 192", got)
	}
}

// The server stops sending for a while mid-song: a reader at playback
// speed doesn't notice, because the relay had read ahead.
func TestRelayRidesOutAServerPause(t *testing.T) {
	fs := newFakeServer(t, 1600<<10)
	var once sync.Once
	fs.setPause(func(id string, sent int) {
		if sent >= 1<<20 {
			once.Do(func() { time.Sleep(1500 * time.Millisecond) })
		}
	})
	r := newTestRelay(t, fs)
	resp, err := http.Get(r.URL(Song{ID: "abc"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(300 * time.Millisecond) // ffplay starting up
	// 24 KB every 50 ms: about 4 times 192 kbit/s, so the test is quick
	buf := make([]byte, 24<<10)
	var longest time.Duration
	var got []byte
	for {
		t0 := time.Now()
		n, err := io.ReadFull(resp.Body, buf)
		longest = max(longest, time.Since(t0))
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !bytes.Equal(got, song("abc", fs.size)) {
		t.Fatalf("got %d bytes, want %d", len(got), fs.size)
	}
	if longest > 300*time.Millisecond {
		t.Errorf("a read waited %v: the pause reached the reader", longest)
	}
}

// The next song is fetched once the playing one has been read, and then
// plays from memory: one request to the server, and it starts at once even
// when the server has become slow to start a stream.
func TestRelayPrefetch(t *testing.T) {
	fs := newFakeServer(t, 300<<10)
	r := newTestRelay(t, fs)
	resp, err := http.Get(r.URL(Song{ID: "first"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	r.Prefetch(Song{ID: "second"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for fs.count("second") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	// from now on the server takes a second to start a stream
	slow := time.Second
	fs.delay.Store(int64(slow))

	t0 := time.Now()
	resp, err = http.Get(r.URL(Song{ID: "second"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > slow/2 {
		t.Errorf("the prefetched song took %v to start", d)
	}
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := append(first, rest...); !bytes.Equal(got, song("second", fs.size)) {
		t.Fatalf("got %d bytes, want %d", len(got), fs.size)
	}
	if n := fs.count("second"); n != 1 {
		t.Errorf("the server was asked for the next song %d times", n)
	}
}

// Prefetching another song drops the one fetched before; a seek (t > 0)
// doesn't use the prefetched stream.
func TestRelayPrefetchReplaced(t *testing.T) {
	fs := newFakeServer(t, 4<<20)
	fs.setPause(func(string, int) { time.Sleep(20 * time.Millisecond) })
	r := newTestRelay(t, fs)
	r.Prefetch(Song{ID: "b"})
	deadline := time.Now().Add(3 * time.Second)
	for fs.count("b") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	r.Prefetch(Song{ID: "c"})
	for {
		fs.mu.Lock()
		gone := fs.cancelled["b"]
		fs.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the replaced prefetch kept its stream open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Get(r.URL(Song{ID: "c"}, 30))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(resp.Body, make([]byte, 1000))
	resp.Body.Close()
	if n := fs.count("c"); n != 2 {
		t.Errorf("a seek into the prefetched song made %d requests, want a new one (2)", n)
	}
}

// Skipping (ffplay closes the stream) closes the server's stream too.
func TestRelaySkipClosesTheStream(t *testing.T) {
	fs := newFakeServer(t, 8<<20)
	fs.setPause(func(string, int) { time.Sleep(20 * time.Millisecond) })
	r := newTestRelay(t, fs)
	resp, err := http.Get(r.URL(Song{ID: "a"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(resp.Body, make([]byte, 100<<10))
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fs.mu.Lock()
		gone := fs.cancelled["a"]
		fs.mu.Unlock()
		if gone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the server's stream stayed open after ffplay left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// play reads song s through the relay to the end.
func play(t *testing.T, r *relay, s Song) []byte {
	t.Helper()
	resp, err := http.Get(r.URL(s, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

// A file passed through as it is (its size known) that stops mid-way is
// picked up with a Range request, and the whole song is kept.
func TestRelayResumesAFile(t *testing.T) {
	fs := newFakeServer(t, 900<<10)
	fs.setCut(true, func(id string, n int) int {
		if n == 1 {
			return 300 << 10
		}
		return 0
	})
	r := newTestRelay(t, fs)
	s := Song{ID: "file", Size: int64(fs.size), BitRate: 128, Suffix: "mp3", Duration: 60}
	if b := play(t, r, s); !bytes.Equal(b, song("file", fs.size)) {
		t.Fatalf("got %d bytes, want the whole %d", len(b), fs.size)
	}
	fs.mu.Lock()
	ranges := fs.ranges["file"]
	fs.mu.Unlock()
	if len(ranges) != 1 || ranges[0] != "bytes=307200-" {
		t.Errorf("Range requests %q, want one from byte 307200", ranges)
	}
	time.Sleep(100 * time.Millisecond)
	if b := play(t, r, s); !bytes.Equal(b, song("file", fs.size)) || fs.count("file") != 2 {
		t.Errorf("not kept whole on the card: %d bytes, %d requests", len(b), fs.count("file"))
	}
}

// A transcoded stream that ends early (cleanly: the length isn't known)
// is picked up at the time it reached, with timeOffset.
func TestRelayResumesAStream(t *testing.T) {
	const secs = 40
	fs := newFakeServer(t, secs*192*125)
	fs.setCut(false, func(id string, n int) int {
		if n == 1 {
			return 10 * 192 * 125 // 10 s, then the stream ends
		}
		return 0
	})
	r := newTestRelay(t, fs)
	b := play(t, r, Song{ID: "tx", Duration: secs, BitRate: 320, Suffix: "flac"})
	if !bytes.Equal(b, song("tx", fs.size)) {
		t.Fatalf("got %d bytes, want %d", len(b), fs.size)
	}
	if q := fs.lastQuery("tx"); !strings.Contains(q, "t=10") {
		t.Errorf("picked up with %q, want t=10", q)
	}
}

// A server that keeps stopping early: after maxResumes tries the song ends
// with what arrived, and nothing is kept on the card.
func TestRelayGivesUp(t *testing.T) {
	fs := newFakeServer(t, 600<<10)
	fs.setCut(true, func(id string, n int) int { return 100 << 10 })
	dir := t.TempDir()
	r := newTestRelayIn(t, fs, dir)
	s := Song{ID: "bad", Size: int64(fs.size), BitRate: 128, Suffix: "mp3", Duration: 40}
	b := play(t, r, s)
	if len(b) >= fs.size || !bytes.Equal(b, song("bad", fs.size)[:len(b)]) {
		t.Fatalf("got %d bytes", len(b))
	}
	if n := fs.count("bad"); n != maxResumes+1 {
		t.Errorf("%d requests, want %d", n, maxResumes+1)
	}
	time.Sleep(100 * time.Millisecond)
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("an incomplete song was kept: %d files", len(ents))
	}
}

// A song whose length isn't known and that ends cleanly is taken as whole.
func TestRelayUnknownLength(t *testing.T) {
	fs := newFakeServer(t, 500<<10)
	r := newTestRelay(t, fs)
	if b := play(t, r, Song{ID: "u"}); len(b) != fs.size {
		t.Fatalf("got %d bytes", len(b))
	}
}

// A server that ignores timeOffset: picking up the stream would play the
// song again from the start, so the song ends where the download stopped.
func TestRelayServerIgnoresTimeOffset(t *testing.T) {
	const secs = 40
	fs := newFakeServer(t, secs*192*125)
	fs.mu.Lock()
	fs.ignoreOffset = true
	fs.mu.Unlock()
	fs.setCut(false, func(id string, n int) int {
		if n == 1 {
			return 10 * 192 * 125
		}
		return 0
	})
	dir := t.TempDir()
	r := newTestRelayIn(t, fs, dir)
	b := play(t, r, Song{ID: "old", Duration: secs, BitRate: 320, Suffix: "flac"})
	if !bytes.Equal(b, song("old", fs.size)[:10*192*125]) {
		t.Fatalf("got %d bytes, want the first %d only", len(b), 10*192*125)
	}
	time.Sleep(100 * time.Millisecond)
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("an incomplete song was kept")
	}
}

func TestID3Size(t *testing.T) {
	// a 257-byte tag: synchsafe 0x00 0x00 0x02 0x01
	if n := id3Size([]byte("ID3\x04\x00\x00\x00\x00\x02\x01rest")); n != 10+257 {
		t.Errorf("id3Size = %d, want 267", n)
	}
	if n := id3Size([]byte("\xff\xfbaudio")); n != 0 {
		t.Errorf("no tag: %d", n)
	}
}

// A song from a slow server starts once some of it has arrived, or after
// prebufferWait at most.
func TestRelayPrebuffers(t *testing.T) {
	fs := newFakeServer(t, 2<<20)
	fs.setPause(func(string, int) { time.Sleep(150 * time.Millisecond) }) // ~430 KB/s
	r := newTestRelay(t, fs)
	t0 := time.Now()
	resp, err := http.Get(r.URL(Song{ID: "slow"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadFull(resp.Body, make([]byte, 1))
	if d := time.Since(t0); d < 400*time.Millisecond || d > prebufferWait {
		t.Errorf("the first byte came after %v: no prebuffering", d)
	}
	r.mu.Lock()
	f := r.active
	r.mu.Unlock()
	f.mu.Lock()
	got := f.w
	f.mu.Unlock()
	if got < prebuffer {
		t.Errorf("started with %d bytes downloaded, want %d", got, prebuffer)
	}
}

// The player's order: it asks for the next song's URL, starts ffplay, and
// prefetches the song after, before ffplay connects. The next song, still
// downloading, must not be dropped and fetched again.
func TestRelayKeepsThePrefetchInThePlayersOrder(t *testing.T) {
	fs := newFakeServer(t, 1<<20)
	fs.setPause(func(string, int) { time.Sleep(15 * time.Millisecond) })
	r := newTestRelay(t, fs)
	one, two, three := Song{ID: "one"}, Song{ID: "two"}, Song{ID: "three"}
	resp, err := http.Get(r.URL(one, 0))
	if err != nil {
		t.Fatal(err)
	}
	r.Prefetch(two)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// song one has ended; two is still downloading
	u := r.URL(two, 0)
	r.Prefetch(three)
	if b := playURL(t, u); !bytes.Equal(b, song("two", fs.size)) {
		t.Fatalf("got %d bytes", len(b))
	}
	if n := fs.count("two"); n != 1 {
		t.Errorf("the server was asked for the next song %d times, want 1", n)
	}
}

func playURL(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

// A song started part way through (as after a game) isn't kept on the
// card as if it were the whole song.
func TestRelayPartwayNotKept(t *testing.T) {
	fs := newFakeServer(t, 40*192*125)
	r := newTestRelay(t, fs)
	s := Song{ID: "g", Duration: 40}
	if b := playURL(t, r.URL(s, 10)); len(b) != fs.size-10*192*125 {
		t.Fatalf("from 10 s: %d bytes", len(b))
	}
	time.Sleep(100 * time.Millisecond)
	if b := play(t, r, s); len(b) != fs.size || fs.count("g") != 2 {
		t.Errorf("from the start: %d bytes after %d requests, want the whole song from the server", len(b), fs.count("g"))
	}
}

// Without the card, a long song is held in memory only a window at a
// time, and still arrives whole.
func TestRelayMemoryWindow(t *testing.T) {
	fs := newFakeServer(t, memAhead+6<<20)
	r := newTestRelayIn(t, fs, "")
	resp, err := http.Get(r.URL(Song{ID: "long"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 100<<10)
	io.ReadFull(resp.Body, first)
	time.Sleep(500 * time.Millisecond)
	r.mu.Lock()
	f := r.active
	r.mu.Unlock()
	f.mu.Lock()
	ahead := f.w - f.r
	f.mu.Unlock()
	if ahead > memAhead+64<<10 {
		t.Errorf("%d bytes held ahead in memory, more than %d", ahead, memAhead)
	}
	rest, _ := io.ReadAll(resp.Body)
	if got := append(first, rest...); !bytes.Equal(got, song("long", fs.size)) {
		t.Fatalf("got %d bytes, want %d", len(got), fs.size)
	}
	m := f.data.(*memData)
	m.mu.Lock()
	held := len(m.b)
	m.mu.Unlock()
	if held > memAhead {
		t.Errorf("%d bytes still held after the song was read", held)
	}
}
