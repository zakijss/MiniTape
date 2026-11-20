package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zakijss/minideck/tnet"
)

// Radio streams through the relay. A station is a live stream: nothing is
// kept on the card and nothing is fetched ahead. The relay asks for the
// song titles (ICY metadata), takes them out of the audio before ffplay
// sees it, and keeps the latest for Now Playing. It holds a few seconds of
// audio as a cushion (stations send a burst at the start), and connects
// again when the stream drops, so a short Wi-Fi drop is a gap, not the end
// of the station.

const (
	// radioChunk is the most audio passed on at once; radioBuffer how many
	// pieces are held (at most 256 KB, about 16 s at 128 kbit/s), and
	// radioPrebuffer how many bytes arrive before ffplay gets the first.
	radioChunk     = 4 << 10
	radioBuffer    = 64
	radioPrebuffer = 64 << 10
	// radioStall is how long a station may send nothing before the stream
	// counts as dropped.
	radioStall = 15 * time.Second
	// radioRetries is how many times in a row a dropped stream is asked for
	// again before the relay gives up (and the player shows the error).
	radioRetries = 5
	// radioSteady: a connection that brought this much audio played, so the
	// retries start counting again after it.
	radioSteady = 256 << 10
)

// radioURL is where ffplay reads station s from.
func (r *relay) radioURL(s Song) string {
	r.note(s)
	return "http://" + r.addr + "/r/" + url.PathEscape(s.ID)
}

// liveTitle is the song the station with id says it is playing, "" if
// none (or it isn't the station playing).
func (r *relay) liveTitle(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.liveID != id {
		return ""
	}
	return r.live
}

func (r *relay) setLive(id, title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.liveID == id {
		r.live = title
	}
}

// serveRadio relays the station with song id to ffplay.
func (r *relay) serveRadio(w http.ResponseWriter, req *http.Request, id string) {
	r.mu.Lock()
	s := r.songs[id]
	if r.active != nil { // a song ffplay was reading
		r.active.stop()
		r.active = nil
	}
	r.liveID, r.live = id, ""
	r.mu.Unlock()
	if s.Radio == "" {
		http.Error(w, "unknown station", http.StatusNotFound)
		return
	}
	ctx := req.Context()
	up, err := r.radioGet(ctx, s.Radio)
	if err != nil {
		log.Printf("station %s: %v", id, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if ct := up.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(http.StatusOK)
	chunks, pumped := make(chan []byte, radioBuffer), make(chan struct{})
	var held atomic.Int64 // bytes in chunks
	go func() {
		r.pumpRadio(ctx, id, s.Radio, up, chunks, &held)
		close(pumped)
	}()
	// the cushion (or as many pieces as are held), unless the station sends
	// it slowly or ends first
	wait := time.After(prebufferWait)
	for held.Load() < radioPrebuffer && len(chunks) < radioBuffer {
		select {
		case <-time.After(100 * time.Millisecond):
			continue
		case <-wait:
		case <-pumped:
		case <-ctx.Done():
			return
		}
		break
	}
	flush, _ := w.(http.Flusher)
	var n int64
	defer func() { log.Printf("station %s: %d bytes", id, n) }()
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				return // the station couldn't be reached again: ffplay ends
			}
			held.Add(-int64(len(c)))
			if _, err := w.Write(c); err != nil {
				return
			}
			n += int64(len(c))
			if flush != nil {
				flush.Flush()
			}
		case <-ctx.Done():
			return
		}
	}
}

// pumpRadio passes the audio of up, and of the connections made again
// after it drops, to out, until ctx is done or the station can't be
// reached radioRetries times in a row. It closes out.
func (r *relay) pumpRadio(ctx context.Context, id, src string, up *upstream, out chan<- []byte, held *atomic.Int64) {
	defer close(out)
	for tries := 0; ; tries++ {
		if up != nil && copyRadio(ctx, up, func(t string) { r.setLive(id, t) }, out, held) >= radioSteady {
			tries = 0
		}
		if ctx.Err() != nil || tries >= radioRetries {
			return
		}
		log.Printf("station %s: the stream dropped; connecting again", id)
		select {
		case <-time.After(time.Duration(tries+1) * time.Second):
		case <-ctx.Done():
			return
		}
		var err error
		if up, err = r.radioGet(ctx, src); err != nil {
			log.Printf("station %s: %v", id, err)
		}
	}
}

// copyRadio passes the audio of one connection to out (counted in held),
// the song titles to live, and reports how many bytes of audio it passed.
func copyRadio(ctx context.Context, up *upstream, live func(string), out chan<- []byte, held *atomic.Int64) int64 {
	defer up.cancel()
	defer up.Body.Close()
	var body io.Reader = up.Body
	if every, _ := strconv.Atoi(up.Header.Get("Icy-Metaint")); every > 0 {
		body = &icyReader{r: up.Body, every: every, left: every, title: live}
	}
	stall := time.AfterFunc(radioStall, up.cancel)
	defer stall.Stop()
	var n int64
	for {
		c := make([]byte, radioChunk)
		got, err := body.Read(c)
		stall.Reset(radioStall)
		if got > 0 {
			n += int64(got)
			held.Add(int64(got))
			select {
			case out <- c[:got]:
			case <-ctx.Done():
				return n
			}
		}
		if err != nil {
			return n
		}
	}
}

// radioGet asks for a station's stream, with its song titles.
func (r *relay) radioGet(ctx context.Context, src string) (*upstream, error) {
	if u, err := url.Parse(src); err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("not a web address")
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, "GET", src, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Icy-MetaData", "1")
	req.Header.Set("User-Agent", userAgent())
	resp, err := r.radio.Do(req)
	if err != nil {
		cancel()
		return nil, tnet.Explain(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, errors.New("the station said " + resp.Status)
	}
	return &upstream{Body: resp.Body, Header: resp.Header, StatusCode: resp.StatusCode, cancel: cancel}, nil
}

// radioClient is the HTTP client for stations: no overall timeout (a
// station plays for hours), and old SHOUTcast servers' "ICY 200 OK" read as
// HTTP (see icyConn).
func radioClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	return &http.Client{Transport: &http.Transport{
		Proxy: tnet.ProxyFor,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &icyConn{Conn: c}, nil
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       20 * time.Second,
	}}
}

// icyConn reads a reply that starts "ICY 200 OK", as SHOUTcast 1 servers
// answer, as "HTTP/1.0 200 OK": Go's HTTP client refuses the first.
// shortcut: only a status line whose first 4 bytes come in the first read,
// and not through a SOCKS proxy (its handshake is the first read); fine for
// stations on the internet.
type icyConn struct {
	net.Conn
	read  bool
	extra []byte // the rewritten start, still to be read
}

func (c *icyConn) Read(p []byte) (int, error) {
	if len(c.extra) > 0 {
		n := copy(p, c.extra)
		c.extra = c.extra[n:]
		return n, nil
	}
	n, err := c.Conn.Read(p)
	if !c.read && n > 0 {
		c.read = true
		if n >= 4 && string(p[:4]) == "ICY " {
			c.extra = append([]byte("HTTP/1.0 "), p[4:n]...)
			return c.Read(p)
		}
	}
	return n, err
}

// icyReader passes on the audio of a stream with ICY metadata and hands
// each StreamTitle to title. After every `every` bytes of audio comes a
// length byte n, then n*16 bytes of metadata (n is 0 when nothing changed).
type icyReader struct {
	r     io.Reader
	every int
	left  int // bytes of audio before the next metadata
	title func(string)
}

func (ir *icyReader) Read(p []byte) (int, error) {
	if ir.left == 0 {
		var n [1]byte
		if _, err := io.ReadFull(ir.r, n[:]); err != nil {
			return 0, err
		}
		if n[0] > 0 {
			meta := make([]byte, int(n[0])*16)
			if _, err := io.ReadFull(ir.r, meta); err != nil {
				return 0, err
			}
			if t, ok := streamTitle(string(meta)); ok {
				ir.title(t)
			}
		}
		ir.left = ir.every
	}
	n, err := ir.r.Read(p[:min(len(p), ir.left)])
	ir.left -= n
	return n, err
}

// streamTitle finds StreamTitle='...'; in ICY metadata. The title may hold
// quotes ("Don't Stop"), so it ends at "';". Titles that aren't UTF-8 are
// read as Latin-1, as many stations send them.
func streamTitle(meta string) (string, bool) {
	_, t, ok := strings.Cut(meta, "StreamTitle='")
	if !ok {
		return "", false
	}
	if end := strings.Index(t, "';"); end >= 0 {
		t = t[:end]
	} else {
		t = strings.TrimRight(t, "\x00")
		t = strings.TrimSuffix(t, "'")
	}
	if !utf8.ValidString(t) {
		r := make([]rune, len(t))
		for i := 0; i < len(t); i++ {
			r[i] = rune(t[i])
		}
		t = string(r)
	}
	return strings.TrimSpace(t), true
}
