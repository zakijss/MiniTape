package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/tnet"
)

// relay serves the server's streams to ffplay on 127.0.0.1. MiniTape fetches
// them with its own HTTP client, so HTTPS (with the bundled CA roots) and
// the tailnet proxy work no matter what the device's old ffplay supports.
//
// It also keeps the music going over a poor Wi-Fi link. The device's ffplay
// buffers only about a second of audio, so the relay downloads each song as
// fast as the link allows, to a file on the SD card, and ffplay reads from
// that: a drop in the link is only heard if the download falls behind the
// music. Once a song is downloaded, the next one in the queue is (so the
// server's start-up time, 2-3 s to begin transcoding, is spent while the
// previous song plays, and the next starts at once). Downloaded songs stay
// in a cache on the card and play again without the network.
//
// When a download falls behind anyway (ffplay waited for data), the next
// songs are asked for at a lower bit rate, one step at a time (see
// qualitySteps); after songs that download quickly, it goes back up.
type relay struct {
	addr    string
	src     func(id string, at, kbps int) string // the server's stream URL
	maxRate func() int                           // the bit rate in Settings
	limit   func() int64                         // the room for songs in Settings
	format  func() string                        // the format in Settings
	client  *http.Client
	store   *cache.Disk // nil: songs are kept in memory only

	mu      sync.Mutex
	songs   map[string]Song // what the server says about each song asked for
	active  *fetch          // the stream ffplay is reading
	pending *fetch          // the song ffplay is about to ask for (taken from next)
	next    *fetch          // the song after it, fetched ahead
	down    int             // quality steps below the setting
	good    int             // songs in a row that downloaded quickly
}

const (
	// relayStall is how long the server may send nothing before the
	// download is given up (ffplay then sees the end and the player moves
	// on).
	relayStall = 30 * time.Second
	// slowWait: when ffplay waits this long for data mid-song, it has
	// likely run out: the music stopped.
	slowWait = time.Second
	// A song that isn't on the card yet starts once prebuffer bytes have
	// arrived (about 10 s of music), or after prebufferWait, so a slow
	// server delays the start a little instead of stopping the music soon
	// after.
	prebuffer     = 256 << 10
	prebufferWait = 4 * time.Second
	// minMeasured: a download smaller than this says little about the
	// link's speed, so it doesn't count towards stepping the bit rate up.
	minMeasured = 256 << 10
	// minFree: with less free space than this on the card, songs are kept
	// in memory only.
	minFree = 1 << 30
)

// errWrite marks a download that stopped because the song couldn't be
// written (the card full or failing), not because the link dropped.
var errWrite = errors.New("can't keep the song")

// qualitySteps are the bit rates to fall back to, below the setting.
var qualitySteps = []int{192, 128, 96}

// maxResumes is how many times a download that stopped early is picked up
// again where it stopped.
const maxResumes = 4

// startRelay starts the relay. Songs are kept in cacheDir ("": memory
// only), up to limit bytes (Settings › Downloaded songs), least recently
// played dropped first.
func startRelay(src func(id string, at, kbps int) string, maxRate func() int, format func() string, cacheDir string, limit func() int64) (*relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	// Connect and header timeouts, but no overall one: songs take minutes.
	client := tnet.Client(20 * time.Second)
	client.Timeout = 0
	r := &relay{addr: ln.Addr().String(), src: src, maxRate: maxRate, format: format, limit: limit, client: client, songs: map[string]Song{}}
	if cacheDir != "" {
		r.store = cache.New(cacheDir)
		r.prune()
	}
	go http.Serve(ln, http.HandlerFunc(r.serve))
	return r, nil
}

// prune drops the least recently played songs beyond the limit, in the
// background.
func (r *relay) prune() {
	if r.store != nil {
		go r.store.Prune(r.limit())
	}
}

// URL is where ffplay reads song s from, starting at seconds in. The
// player asks for it before starting ffplay, and then prefetches the song
// after: a fetch of s already under way is set aside for ffplay here, so
// that prefetch doesn't replace it.
func (r *relay) URL(s Song, at int) string {
	r.note(s)
	r.mu.Lock()
	if n := r.next; n != nil && n.id == s.ID && at == 0 {
		if r.pending != nil {
			r.pending.stop()
		}
		r.pending, r.next = n, nil
	}
	r.mu.Unlock()
	return "http://" + r.addr + "/s/" + url.PathEscape(s.ID) + "?t=" + strconv.Itoa(at)
}

// note keeps what the server said about a song (its size and length), to
// tell a whole download from one that stopped early.
func (r *relay) note(s Song) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.songs) > 500 { // a few queues' worth; it only needs the coming songs
		clear(r.songs)
	}
	r.songs[s.ID] = s
}

// rates are the bit rates in use: the setting, then the lower steps.
func (r *relay) rates() []int {
	top := r.maxRate()
	rs := []int{top}
	for _, q := range qualitySteps {
		if q < top {
			rs = append(rs, q)
		}
	}
	return rs
}

// rate is the bit rate to ask for now. The caller holds mu.
func (r *relay) rate() int {
	rs := r.rates()
	return rs[min(r.down, len(rs)-1)]
}

func (r *relay) serve(w http.ResponseWriter, req *http.Request) {
	id := strings.TrimPrefix(req.URL.Path, "/s/")
	at, _ := strconv.Atoi(req.URL.Query().Get("t"))
	r.mu.Lock()
	var f *fetch
	switch {
	case at != 0:
	case r.pending != nil && r.pending.id == id:
		f, r.pending = r.pending, nil // the song fetched ahead: use it
	case r.next != nil && r.next.id == id:
		f, r.next = r.next, nil
	}
	if f == nil {
		f = r.start(id, at)
	}
	if r.active != nil && r.active != f {
		r.active.stop()
	}
	r.active = f
	f.attach()
	r.mu.Unlock()
	defer func() {
		f.stop() // ffplay is done with it (end, skip, stop)
		f.release(&f.readerDone)
		r.mu.Lock()
		if r.active == f {
			r.active = nil
		}
		r.mu.Unlock()
	}()

	select {
	case <-f.ready:
	case <-req.Context().Done():
		return
	}
	if f.hdrErr != nil {
		http.Error(w, f.hdrErr.Error(), http.StatusBadGateway)
		return
	}
	if f.ctype != "" {
		w.Header().Set("Content-Type", f.ctype)
	}
	if f.clen != "" {
		w.Header().Set("Content-Length", f.clen)
	}
	w.WriteHeader(f.status)
	f.waitFor(req.Context(), prebuffer, prebufferWait)
	n, waited, err := f.copyTo(req.Context(), w, func() { r.starved(f) })
	from := "server"
	if f.cached {
		from = "card"
	}
	log.Printf("stream %s at %d kbit/s from the %s: HTTP %d, %d bytes, waited %.1f s, err=%v",
		id, f.rate, from, f.status, n, waited.Seconds(), err)
}

// Prefetch starts fetching song s ahead, for when it plays next. The
// download waits until the playing song's has finished, so the two don't
// share the link.
func (r *relay) Prefetch(s Song) {
	r.note(s)
	id := s.ID
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next != nil {
		if r.next.id == id && r.next.at == 0 {
			return
		}
		r.next.stop()
	}
	// after the song ffplay is about to play, or the one it plays
	after := r.pending
	if after == nil {
		after = r.active
	}
	r.next = r.start(id, 0, after)
}

// playingRate is the bit rate of the song ffplay is reading, 0 if none.
func (r *relay) playingRate() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return 0
	}
	return r.active.rate
}

// close stops every stream, as the player exits.
func (r *relay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range []*fetch{r.active, r.pending, r.next} {
		if f != nil {
			f.stop()
		}
	}
	r.active, r.pending, r.next = nil, nil, nil
}

// starved is called when ffplay ran out of data mid-song: the songs after
// the next are asked for one step lower. A next song already being fetched
// is kept: dropping it would mean waiting for the server between songs.
func (r *relay) starved(f *fetch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.good = 0
	if f.rate != r.rate() || r.down >= len(r.rates())-1 {
		return
	}
	r.down++
	log.Printf("the link fell behind the music: next songs at %d kbit/s", r.rate())
}

// downloaded is called when a song has been read from the server whole: a
// quick download (four times the bit rate or more) counts towards going
// back up a step.
func (r *relay) downloaded(f *fetch, kbps float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.rate != r.rate() || f.wasStarved() {
		return
	}
	if kbps < 4*float64(f.rate) {
		r.good = 0
		return
	}
	if r.good++; r.good >= 2 && r.down > 0 {
		r.down--
		r.good = 0
		log.Printf("the link is quick again: next songs at %d kbit/s", r.rate())
	}
}

// cacheKey names a downloaded song: its id and the bit rate it was
// fetched at.
func cacheKey(id string, kbps int) string { return fmt.Sprintf("stream/%s@%d", id, kbps) }

// start begins fetching song id from at seconds, once the fetches in after
// have finished downloading. The caller holds mu.
func (r *relay) start(id string, at int, after ...*fetch) *fetch {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fetch{id: id, at: at, rate: r.rate(), song: r.songs[id], ready: make(chan struct{}), ctx: ctx, cancel: cancel}
	if r.format != nil {
		f.format = r.format()
	}
	f.cond = sync.NewCond(&f.mu)
	// A song on the card plays from there, at the best rate it was kept at.
	if at == 0 && r.store != nil {
		for _, kbps := range r.rates() {
			if file, ok := r.store.Open(cacheKey(id, kbps)); ok {
				f.fromFile(file, kbps)
				return f
			}
		}
	}
	go func() {
		for _, a := range after {
			if a != nil {
				a.waitDone(ctx)
			}
		}
		f.run(r)
	}()
	return f
}

// data is where a song is kept while it downloads: a file on the card, or
// memory.
type data interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// memData is data in memory, for when the card can't take it. It keeps a
// window of the song: the download waits while memAhead bytes are unread,
// and what ffplay has read is dropped (drop), so a long song can't fill
// the device's memory.
type memData struct {
	mu   sync.Mutex
	b    []byte
	base int64 // the song's offset of b[0]
}

// memAhead is how much of a song memory holds ahead of ffplay (about 5
// minutes at 192 kbit/s).
const memAhead = 8 << 20

func (m *memData) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off != m.base+int64(len(m.b)) {
		return 0, errors.New("memData: writes must be in order")
	}
	m.b = append(m.b, p...)
	return len(p), nil
}

func (m *memData) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off < m.base {
		return 0, errors.New("memData: that part was already dropped")
	}
	if off-m.base >= int64(len(m.b)) {
		return 0, io.EOF
	}
	return copy(p, m.b[off-m.base:]), nil
}

// drop lets go of the song before offset off, once that is 1 MB or more.
func (m *memData) drop(off int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := off - m.base; n >= 1<<20 && n <= int64(len(m.b)) {
		m.b = append([]byte(nil), m.b[n:]...)
		m.base = off
	}
}

func (m *memData) Close() error { return nil }

// fetch is one song: downloading from the server (or read from the card)
// into data, for one reader, ffplay.
type fetch struct {
	id     string
	at     int
	rate   int
	song   Song          // as the server describes it (may be empty)
	format string        // the format asked for
	ready  chan struct{} // closed once the server answered (or failed)
	// status, ctype, clen and hdrErr are set before ready is closed
	status int
	ctype  string
	clen   string
	hdrErr error
	cached bool // read from the card, not the server

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	data    data
	tmp     string // the file being downloaded to, until it is kept
	w       int64  // bytes downloaded
	r       int64  // bytes ffplay has read
	done    bool   // the download ended (err says how)
	err     error
	stopped bool
	starved bool
	// the data is closed (and an unkept file removed) once both the
	// download and the reader are done with it
	attached, fillerDone, readerDone bool
}

// fromFile makes f serve a song already on the card.
func (f *fetch) fromFile(file *os.File, kbps int) {
	f.data, f.cached, f.rate, f.status = file, true, kbps, http.StatusOK
	if fi, err := file.Stat(); err == nil {
		f.w = fi.Size()
		f.clen = strconv.FormatInt(f.w, 10)
	}
	f.done, f.fillerDone = true, true
	close(f.ready)
}

func (f *fetch) run(r *relay) {
	defer func() {
		f.mu.Lock()
		f.done = true
		f.cond.Broadcast()
		f.mu.Unlock()
		f.release(&f.fillerDone)
	}()
	up, err := f.get(r, f.at, -1)
	if err != nil {
		f.fail(err)
		return
	}
	// To the card, when there's room; else to memory.
	var d data = &memData{}
	tmp := ""
	if r.store != nil && up.StatusCode == http.StatusOK && freeSpace(r.store) {
		if tf, err := r.store.Temp(); err == nil {
			d, tmp = tf, tf.Name()
		}
	}
	f.mu.Lock()
	f.data, f.tmp = d, tmp
	f.mu.Unlock()
	// No Content-Length: a download picked up again may come to a
	// different total than the server first said. ffplay doesn't need it.
	f.status, f.ctype = up.StatusCode, up.Header.Get("Content-Type")
	close(f.ready)
	start := time.Now()
	f.fill(up)
	if f.status != http.StatusOK {
		return
	}
	// A download that stopped early (the server or the link gave up) is
	// picked up where it stopped.
	for attempt := 1; ; attempt++ {
		f.mu.Lock()
		stopped, n, ferr := f.stopped, f.w, f.err
		f.mu.Unlock()
		if stopped {
			return
		}
		at, from, short := f.shortOf(n)
		if !short && ferr == nil {
			break
		}
		if errors.Is(ferr, errWrite) {
			log.Printf("stream %s: %v; it ends here", f.id, ferr)
			return // asking the server again wouldn't help
		}
		if attempt > maxResumes {
			log.Printf("stream %s: the download stopped at %d bytes and couldn't be picked up again (%v)", f.id, n, ferr)
			return // ffplay plays what there is; nothing is kept
		}
		log.Printf("stream %s: the download stopped at %d bytes (%v); picking it up again", f.id, n, ferr)
		up, err := f.get(r, at, from)
		if err != nil {
			f.mu.Lock()
			f.err = err
			f.mu.Unlock()
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		if from < 0 && f.restarted(up) {
			log.Printf("stream %s: the server ignores timeOffset and sent the song from the start; it ends here", f.id)
			up.Body.Close()
			up.cancel()
			return
		}
		f.mu.Lock()
		f.err = nil
		f.mu.Unlock()
		f.fill(up)
	}
	f.mu.Lock()
	n := f.w
	f.mu.Unlock()
	if secs := time.Since(start).Seconds(); secs > 0 && n > minMeasured {
		r.downloaded(f, float64(n)*8/1000/secs)
	}
	// Keep the whole song on the card for next time (not one started part
	// way through, as after a game).
	if tmp != "" && f.at == 0 && r.store.Commit(tmp, cacheKey(f.id, f.rate)) == nil {
		f.mu.Lock()
		f.tmp = ""
		f.mu.Unlock()
		r.prune()
	}
}

// passedThrough reports whether the server sends the song's file as it is:
// Navidrome does when it is already in the format asked for, at or below
// the bit rate. Then its size is known exactly.
func (f *fetch) passedThrough() bool {
	s := f.song
	if f.at != 0 || s.Size <= 0 {
		return false
	}
	return f.format == "raw" || s.BitRate > 0 && s.BitRate <= f.rate && f.format != "" && strings.EqualFold(s.Suffix, f.format)
}

// shortOf reports whether n bytes are less than the whole song, and where
// to pick it up: from byte from (a file passed through, with a Range
// request), or else at seconds at (a transcoded stream, with timeOffset).
func (f *fetch) shortOf(n int64) (at int, from int64, short bool) {
	if f.passedThrough() {
		return 0, n, n < f.song.Size
	}
	at = f.at + int(float64(n)/(float64(f.rate)*125))
	// Only MP3 is transcoded at a constant rate, so only there does the
	// size tell how much of the song arrived. Otherwise only an error says
	// the download stopped early.
	if f.song.Duration <= 0 || !strings.EqualFold(f.format, "mp3") {
		return at, -1, false
	}
	want := float64(f.song.Duration-f.at) * float64(f.rate) * 125
	return at, -1, float64(n) < want*0.9
}

// restarted reports whether a stream asked for from a later time came back
// from the start instead (a server that ignores timeOffset): its audio,
// past the tags, matches the start of the song already downloaded. What it
// read is put back in front of the body.
func (f *fetch) restarted(up *upstream) bool {
	const window = 16 << 10
	head := make([]byte, 64<<10)
	stall := time.AfterFunc(relayStall, up.cancel)
	n, _ := io.ReadFull(up.Body, head)
	stall.Stop()
	head = head[:n]
	up.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), up.Body), up.Body}
	f.mu.Lock()
	d, have := f.data, f.w
	f.mu.Unlock()
	start := id3Size(head) + 4096 // past the tags and the first frames
	if int64(start+window) > have || start+window > len(head) {
		return false
	}
	first := make([]byte, window)
	if _, err := d.ReadAt(first, int64(start)); err != nil {
		return false
	}
	return bytes.Equal(first, head[start:start+window])
}

// id3Size is the length of the ID3v2 tag at the start of b, or 0.
func id3Size(b []byte) int {
	if len(b) < 10 || string(b[:3]) != "ID3" {
		return 0
	}
	return 10 + (int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f))
}

// upstream is one request to the server, with its stall timer's cancel.
type upstream struct {
	Body       io.ReadCloser
	Header     http.Header
	StatusCode int
	cancel     context.CancelFunc
}

// get asks the server for the song from at seconds, or, with from >= 0,
// for its file from byte from (which must come back as 206).
func (f *fetch) get(r *relay, at int, from int64) (*upstream, error) {
	ctx, cancel := context.WithCancel(f.ctx)
	req, err := http.NewRequestWithContext(ctx, "GET", r.src(f.id, at, f.rate), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if from >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	up, err := r.client.Do(req)
	if err != nil {
		cancel()
		return nil, tnet.Explain(err)
	}
	if from >= 0 && up.StatusCode != http.StatusPartialContent {
		up.Body.Close()
		cancel()
		return nil, fmt.Errorf("the server can't continue the file from byte %d (HTTP %d)", from, up.StatusCode)
	}
	return &upstream{Body: up.Body, Header: up.Header, StatusCode: up.StatusCode, cancel: cancel}, nil
}

func (f *fetch) fail(err error) {
	f.hdrErr, f.err = err, err
	close(f.ready)
}

// fill downloads the server's stream into data, as fast as it comes, and
// closes it.
func (f *fetch) fill(up *upstream) {
	defer up.cancel()
	defer up.Body.Close()
	body := up.Body
	// The timer runs only during reads from the server; a stall ends this
	// request, not the fetch (it can be picked up again).
	stall := time.AfterFunc(relayStall, up.cancel)
	stall.Stop()
	buf := make([]byte, 32<<10)
	for {
		if !f.roomInMemory() {
			return
		}
		stall.Reset(relayStall)
		got, err := body.Read(buf)
		stall.Stop()
		if got > 0 {
			f.mu.Lock()
			off := f.w
			f.mu.Unlock()
			// Only this goroutine writes, at the end; the reader reads
			// [0, w), so there is no overlap.
			if _, werr := f.data.WriteAt(buf[:got], off); werr != nil {
				err = fmt.Errorf("%w: %v", errWrite, werr)
			} else {
				f.mu.Lock()
				f.w += int64(got)
				f.cond.Broadcast()
				f.mu.Unlock()
			}
		}
		if err != nil {
			f.mu.Lock()
			if !errors.Is(err, io.EOF) {
				f.err = err
			}
			f.mu.Unlock()
			return
		}
		f.mu.Lock()
		stopped := f.stopped
		f.mu.Unlock()
		if stopped {
			return
		}
	}
}

// roomInMemory waits, for a song kept in memory, until fewer than memAhead
// bytes are unread. It reports false if the fetch was stopped meanwhile.
func (f *fetch) roomInMemory() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data.(*memData); ok {
		for f.w-f.r >= memAhead && !f.stopped {
			f.cond.Wait()
		}
	}
	return !f.stopped
}

// copyTo writes the song to w until it ends, ctx is done or the fetch is
// stopped. It reports the bytes written and the time ffplay spent waiting
// for the download; starved is called each time a wait mid-song lasts
// slowWait or more.
func (f *fetch) copyTo(ctx context.Context, w io.Writer, starved func()) (int64, time.Duration, error) {
	stopWake := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.stopped = true
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer stopWake()
	var pos int64
	var waited time.Duration
	buf := make([]byte, 32<<10)
	for {
		f.mu.Lock()
		var since time.Time
		for pos == f.w && !f.done && !f.stopped {
			if since.IsZero() {
				since = time.Now()
			}
			f.cond.Wait()
		}
		avail, stopped, err, d := f.w-pos, f.stopped, f.err, f.data
		f.mu.Unlock()
		if !since.IsZero() {
			wait := time.Since(since)
			waited += wait
			if wait >= slowWait && pos > 0 && !stopped {
				log.Printf("stream %s: the music waited %.1f s for the download", f.id, wait.Seconds())
				f.mu.Lock()
				f.starved = true
				f.mu.Unlock()
				starved()
			}
		}
		if stopped {
			return pos, waited, context.Canceled
		}
		if avail == 0 {
			return pos, waited, err
		}
		n, rerr := d.ReadAt(buf[:min(int64(len(buf)), avail)], pos)
		if n > 0 {
			got, werr := w.Write(buf[:n])
			pos += int64(got)
			f.mu.Lock()
			f.r = pos
			f.cond.Broadcast()
			f.mu.Unlock()
			if m, ok := d.(*memData); ok {
				m.drop(pos)
			}
			if werr != nil {
				return pos, waited, werr
			}
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return pos, waited, rerr
		}
	}
}

func (f *fetch) wasStarved() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starved
}

// attach notes that ffplay is reading this fetch.
func (f *fetch) attach() {
	f.mu.Lock()
	f.attached = true
	f.mu.Unlock()
}

// stop ends the fetch: the download stops and the reader, if any, returns.
// A fetch nobody reads (a replaced prefetch) lets go of its data now.
func (f *fetch) stop() {
	f.mu.Lock()
	f.stopped = true
	attached := f.attached
	f.cond.Broadcast()
	f.mu.Unlock()
	f.cancel()
	if !attached {
		f.release(&f.readerDone)
	}
}

// release marks one side (the download or the reader) as done with the
// data; after both, it is closed, and a download that wasn't kept is
// deleted.
func (f *fetch) release(side *bool) {
	f.mu.Lock()
	if *side {
		f.mu.Unlock()
		return
	}
	*side = true
	both := f.fillerDone && f.readerDone
	d, tmp := f.data, f.tmp
	f.mu.Unlock()
	if !both {
		return
	}
	if d != nil {
		d.Close()
	}
	if tmp != "" {
		os.Remove(tmp)
	}
}

// waitFor returns once n bytes have been downloaded (or all of the song),
// after d, or when ctx is done.
func (f *fetch) waitFor(ctx context.Context, n int64, d time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	stopWake := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer stopWake()
	f.mu.Lock()
	for f.w < n && !f.done && !f.stopped && ctx.Err() == nil {
		f.cond.Wait()
	}
	f.mu.Unlock()
}

// waitDone returns once the fetch has finished downloading (or failed, or
// was stopped), or ctx is done.
func (f *fetch) waitDone(ctx context.Context) {
	stopWake := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer stopWake()
	f.mu.Lock()
	for !f.done && !f.stopped && ctx.Err() == nil {
		f.cond.Wait()
	}
	f.mu.Unlock()
}

// freeSpace reports whether the card has room for more songs.
func freeSpace(c *cache.Disk) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(c.Dir(), &st); err != nil {
		return false
	}
	return int64(st.Bavail)*int64(st.Bsize) >= minFree
}
