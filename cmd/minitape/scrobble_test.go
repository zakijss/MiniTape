package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zakijss/minideck/qr"
	"github.com/zakijss/minideck/secrets"
)

const (
	testKey    = "b25b959554ed76058ac220b7b2e0a026"
	testSecret = "secret0123456789abcdef0123456789"
	testToken  = "0123456789abcdef0123456789abcdef"
)

// The signature of a known request, worked out by hand with md5(1):
// md5("api_key" KEY "method" "auth.getSession" "token" TOKEN SECRET).
// format isn't signed, and the order the parameters come in doesn't
// matter.
func TestLastFMSignature(t *testing.T) {
	p := url.Values{"token": {testToken}, "method": {"auth.getSession"}, "api_key": {testKey}, "format": {"json"}}
	if got, want := lastfmSign(p, testSecret), "056e17778dcb5ffd6fec09bd662a4756"; got != want {
		t.Errorf("api_sig = %s, want %s", got, want)
	}
}

// The approval link fits the QR encoder (it takes up to 106 bytes) with
// Last.fm's 32-character key and token.
func TestLastFMAuthURLFitsQR(t *testing.T) {
	link := lastfmAuthURL(testKey, testToken)
	if !strings.HasPrefix(link, "https://last.fm/api/auth/?api_key="+testKey+"&token=") {
		t.Errorf("link %s", link)
	}
	if _, err := qr.Encode(link); err != nil {
		t.Errorf("%d bytes: %v", len(link), err)
	}
}

// fakeLastFM is a Last.fm API server: it checks every request's signature
// and records the requests; reply answers them.
type fakeLastFM struct {
	t     *testing.T
	mu    sync.Mutex
	calls []url.Values
	reply func(p url.Values) (status int, body string)
}

func (f *fakeLastFM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("%s, want POST", r.Method)
	}
	if err := r.ParseForm(); err != nil {
		f.t.Fatal(err)
	}
	p := r.PostForm
	sig := p.Get("api_sig")
	unsigned := url.Values{}
	for k, v := range p {
		if k != "api_sig" {
			unsigned[k] = v
		}
	}
	if want := lastfmSign(unsigned, testSecret); sig != want {
		f.t.Errorf("%s: api_sig %s, want %s", p.Get("method"), sig, want)
	}
	if p.Get("format") != "json" || p.Get("api_key") != testKey {
		f.t.Errorf("format %q api_key %q", p.Get("format"), p.Get("api_key"))
	}
	f.mu.Lock()
	f.calls = append(f.calls, p)
	f.mu.Unlock()
	status, body := http.StatusOK, `{}`
	if f.reply != nil {
		status, body = f.reply(p)
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "120")
	}
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (f *fakeLastFM) Calls() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.calls...)
}

func newFakeLastFM(t *testing.T) (*fakeLastFM, *LastFM) {
	f := &fakeLastFM{t: t}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &LastFM{Base: srv.URL + "/2.0/", Key: testKey, Secret: testSecret, Session: "sk1", HTTP: srv.Client()}
}

var song1 = Listen{Artist: "The Static Gardens", Track: "Signal Fires", Album: "Night Signals", Duration: 214, Number: 1, Started: 1793000000}
var song2 = Listen{Artist: "Paper Satellites", Track: "Low Orbit", Duration: 187, Started: 1793000300}

func TestLastFMRequests(t *testing.T) {
	f, lf := newFakeLastFM(t)
	ctx := context.Background()
	if err := lf.NowPlaying(ctx, song1); err != nil {
		t.Fatal(err)
	}
	if err := lf.Submit(ctx, []Listen{song1, song2}, true); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	np, sc := calls[0], calls[1]
	want := map[string]string{"method": "track.updateNowPlaying", "sk": "sk1", "artist": song1.Artist,
		"track": song1.Track, "album": song1.Album, "duration": "214", "trackNumber": "1"}
	for k, v := range want {
		if np.Get(k) != v {
			t.Errorf("now playing %s = %q, want %q", k, np.Get(k), v)
		}
	}
	want = map[string]string{"method": "track.scrobble", "sk": "sk1",
		"artist[0]": song1.Artist, "track[0]": song1.Track, "album[0]": song1.Album, "timestamp[0]": "1793000000",
		"duration[0]": "214", "trackNumber[0]": "1",
		"artist[1]": song2.Artist, "track[1]": song2.Track, "timestamp[1]": "1793000300", "duration[1]": "187"}
	for k, v := range want {
		if sc.Get(k) != v {
			t.Errorf("scrobble %s = %q, want %q", k, sc.Get(k), v)
		}
	}
	if sc.Has("album[1]") || sc.Has("trackNumber[1]") {
		t.Error("empty album or track number sent")
	}
}

// Last.fm's error codes (and plain HTTP errors) sort into retry, drop and
// log in again; a 429's Retry-After is kept.
func TestLastFMErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   errKind
		after  time.Duration
	}{
		{200, `{"error":9,"message":"Invalid session key - Please re-authenticate"}`, errLogin, 0},
		{403, `{"error":10,"message":"Invalid API key"}`, errLogin, 0},
		{200, `{"error":11,"message":"Service Offline"}`, errTemporary, 0},
		{200, `{"error":16,"message":"There was a temporary error"}`, errTemporary, 0},
		{200, `{"error":6,"message":"Invalid parameters"}`, errRejected, 0},
		{429, `{"error":29,"message":"Rate limit exceeded"}`, errTemporary, 120 * time.Second},
		{429, `slow down`, errTemporary, 120 * time.Second},
		{503, `<html>`, errTemporary, 0},
	}
	for _, c := range cases {
		f, lf := newFakeLastFM(t)
		f.reply = func(url.Values) (int, string) { return c.status, c.body }
		err := lf.Submit(context.Background(), []Listen{song1}, false)
		if err == nil {
			t.Errorf("%d %s: no error", c.status, c.body)
			continue
		}
		if kindOf(err) != c.kind || retryAfterOf(err) != c.after {
			t.Errorf("%d %s: kind %d retry %v, want %d %v", c.status, c.body, kindOf(err), retryAfterOf(err), c.kind, c.after)
		}
		if strings.Contains(err.Error(), "sk1") || strings.Contains(err.Error(), testSecret) {
			t.Errorf("error shows a secret: %v", err)
		}
	}
	// The network down: temporary.
	lf := &LastFM{Base: "http://127.0.0.1:1/", Key: testKey, Secret: testSecret, HTTP: http.DefaultClient}
	if err := lf.NowPlaying(context.Background(), song1); kindOf(err) != errTemporary {
		t.Errorf("no server: %v", err)
	}
}

// The login: a token, then the session once the user approves it (until
// then Last.fm answers error 14).
func TestLastFMLogin(t *testing.T) {
	f, lf := newFakeLastFM(t)
	lf.Session = ""
	asks := 0
	f.reply = func(p url.Values) (int, string) {
		switch p.Get("method") {
		case "auth.getToken":
			return 200, `{"token":"` + testToken + `"}`
		case "auth.getSession":
			if p.Get("token") != testToken {
				t.Errorf("token %q", p.Get("token"))
			}
			if asks++; asks < 3 {
				return 403, `{"error":14,"message":"Unauthorized Token - This token has not been authorized"}`
			}
			return 200, `{"session":{"name":"alex","key":"d580d57f32848f5dcf574d1ce18d78b2","subscriber":0}}`
		}
		return 400, `{"error":3,"message":"Invalid Method"}`
	}
	tok, err := lf.Token(context.Background())
	if err != nil || tok != testToken {
		t.Fatalf("token %q %v", tok, err)
	}
	name, key, err := lf.pollSession(context.Background(), tok, time.Millisecond)
	if err != nil || name != "alex" || key != "d580d57f32848f5dcf574d1ce18d78b2" || asks != 3 {
		t.Fatalf("session %q %q %v after %d asks", name, key, err, asks)
	}
	for _, c := range f.Calls() {
		if c.Has("sk") {
			t.Error("login calls sent a session key")
		}
	}

	// An expired token ends the wait.
	f.reply = func(url.Values) (int, string) { return 403, `{"error":15,"message":"This token has expired"}` }
	if _, _, err := lf.pollSession(context.Background(), tok, time.Millisecond); kindOf(err) != errLogin {
		t.Errorf("expired token: %v", err)
	}
	// Giving up (B, or 5 minutes) ends it too.
	f.reply = func(url.Values) (int, string) { return 403, `{"error":14,"message":"Unauthorized Token"}` }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := lf.pollSession(ctx, tok, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
}

// fakeLB is a ListenBrainz server that records the submitted payloads.
type fakeLB struct {
	t        *testing.T
	mu       sync.Mutex
	payloads []map[string]any
	status   int
	header   http.Header
	valid    bool
}

func (f *fakeLB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Token lb-token-1234567890abcdef" {
		f.t.Errorf("Authorization %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.header {
		w.Header()[k] = v
	}
	switch r.URL.Path {
	case "/1/validate-token":
		if r.Method != http.MethodGet {
			f.t.Errorf("validate: %s", r.Method)
		}
		if f.valid {
			fmt.Fprint(w, `{"code":200,"message":"Token valid.","valid":true,"user_name":"alex"}`)
		} else {
			fmt.Fprint(w, `{"code":200,"message":"Token invalid.","valid":false}`)
		}
	case "/1/submit-listens":
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			f.t.Fatal(err)
		}
		f.payloads = append(f.payloads, p)
		if f.status != 0 && f.status != 200 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, `{"code":400,"error":"bad listen"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	default:
		w.WriteHeader(404)
	}
}

func newFakeLB(t *testing.T) (*fakeLB, *ListenBrainz) {
	f := &fakeLB{t: t, valid: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &ListenBrainz{Base: srv.URL, Token: "lb-token-1234567890abcdef", HTTP: srv.Client()}
}

func TestListenBrainzRequests(t *testing.T) {
	f, lb := newFakeLB(t)
	ctx := context.Background()
	if user, err := lb.Validate(ctx); err != nil || user != "alex" {
		t.Fatalf("validate: %q %v", user, err)
	}
	if err := lb.NowPlaying(ctx, song1); err != nil {
		t.Fatal(err)
	}
	if err := lb.Submit(ctx, []Listen{song1}, false); err != nil {
		t.Fatal(err)
	}
	if err := lb.Submit(ctx, []Listen{song1, song2}, true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	b, _ := json.Marshal(f.payloads)
	f.mu.Unlock()
	var got []struct {
		ListenType string `json:"listen_type"`
		Payload    []struct {
			ListenedAt *int64 `json:"listened_at"`
			Meta       struct {
				Artist  string         `json:"artist_name"`
				Track   string         `json:"track_name"`
				Release string         `json:"release_name"`
				Info    map[string]any `json:"additional_info"`
			} `json:"track_metadata"`
		} `json:"payload"`
	}
	json.Unmarshal(b, &got)
	if len(got) != 3 {
		t.Fatalf("%d submissions", len(got))
	}
	if got[0].ListenType != "playing_now" || got[0].Payload[0].ListenedAt != nil {
		t.Errorf("now playing: %s", b)
	}
	if got[1].ListenType != "single" || len(got[1].Payload) != 1 || got[1].Payload[0].ListenedAt == nil || *got[1].Payload[0].ListenedAt != song1.Started {
		t.Errorf("single: %s", b)
	}
	if got[2].ListenType != "import" || len(got[2].Payload) != 2 || *got[2].Payload[1].ListenedAt != song2.Started {
		t.Errorf("import: %s", b)
	}
	m := got[1].Payload[0].Meta
	if m.Artist != song1.Artist || m.Track != song1.Track || m.Release != song1.Album ||
		m.Info["duration_ms"] != float64(214000) || m.Info["tracknumber"] != float64(1) || m.Info["media_player"] != "MiniTape" {
		t.Errorf("metadata: %+v", m)
	}

	f.mu.Lock()
	f.valid = false
	f.mu.Unlock()
	if _, err := lb.Validate(ctx); kindOf(err) != errLogin {
		t.Errorf("invalid token: %v", err)
	}
}

func TestListenBrainzErrors(t *testing.T) {
	cases := []struct {
		status int
		header http.Header
		kind   errKind
		after  time.Duration
	}{
		{401, nil, errLogin, 0},
		{400, nil, errRejected, 0},
		{429, http.Header{"Retry-After": {"30"}}, errTemporary, 30 * time.Second},
		{429, http.Header{"X-Ratelimit-Reset-In": {"7"}}, errTemporary, 7 * time.Second},
		{500, nil, errTemporary, 0},
	}
	for _, c := range cases {
		f, lb := newFakeLB(t)
		f.status, f.header = c.status, c.header
		err := lb.Submit(context.Background(), []Listen{song1}, false)
		if kindOf(err) != c.kind || retryAfterOf(err) != c.after {
			t.Errorf("%d: %v kind %d retry %v, want %d %v", c.status, err, kindOf(err), retryAfterOf(err), c.kind, c.after)
		}
		if err != nil && strings.Contains(err.Error(), "lb-token") {
			t.Errorf("error shows the token: %v", err)
		}
	}
}

// ---- the queue ----

// fakeAPI is a scrobbling service in memory: err is what each request
// gets, unless refuse says otherwise for a batch.
type fakeAPI struct {
	mu     sync.Mutex
	err    error
	refuse func(ls []Listen) error
	now    int
	sent   [][]Listen
}

func (f *fakeAPI) NowPlaying(ctx context.Context, l Listen) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now++
	return f.err
}

func (f *fakeAPI) Submit(ctx context.Context, ls []Listen, queued bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		if err := f.refuse(ls); err != nil {
			return err
		}
	}
	if f.err == nil {
		f.sent = append(f.sent, ls)
	}
	return f.err
}

func (f *fakeAPI) nowPlayings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeAPI) set(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeAPI) count() (requests, listens int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.sent {
		listens += len(b)
	}
	return len(f.sent), listens
}

// clock is a time the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestScrobbler(t *testing.T, path string, at time.Time) (*Scrobbler, *clock) {
	c := &clock{t: at}
	return newScrobbler(path, c.now), c
}

var errOffline = &sendError{kind: errTemporary, msg: "test: offline"}

// A listen that can't be sent waits in the queue file and is retried with
// a growing wait while music plays, then sent once the network is back.
func TestScrobblerQueuesAndRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, scrobbleQueueFile)
	start := time.Unix(1793000000, 0)
	s, clk := newTestScrobbler(t, path, start)
	api := &fakeAPI{err: errOffline}
	s.Configure([]*service{{name: svcLastFM, api: api, batch: lastfmBatch}}, false)

	l := song1
	l.Started = start.Unix()
	s.Scrobble(l)
	s.Wait()
	if q := readQueueFile(path); len(q.Listens) != 1 || q.Listens[0].Listen != l || q.Listens[0].Service != svcLastFM {
		t.Fatalf("queue file: %+v", q)
	}
	// Read back by the next run of the player.
	if s2, _ := newTestScrobbler(t, path, start); s2.Pending() != 1 {
		t.Fatalf("reloaded queue: %d", s2.Pending())
	}

	tick := func(d time.Duration, playing bool) {
		clk.add(d)
		s.Tick(playing)
		s.Wait()
	}
	tick(time.Minute, true) // the first wait is 2 minutes
	if r, _ := api.count(); r != 0 {
		t.Fatal("retried too soon")
	}
	api.set(nil)
	tick(2*time.Minute, false) // not while the music is stopped
	if r, _ := api.count(); r != 0 {
		t.Fatal("retried with nothing playing")
	}
	api.set(errOffline)
	tick(0, true) // still offline: wait 4 minutes now
	if s.Pending() != 1 {
		t.Fatal("lost the listen")
	}
	api.set(nil)
	tick(3*time.Minute, true)
	if r, _ := api.count(); r != 0 {
		t.Fatal("the wait didn't grow")
	}
	tick(time.Minute, true)
	if r, n := api.count(); r != 1 || n != 1 || s.Pending() != 0 {
		t.Fatalf("sent %d requests, %d listens; %d left", r, n, s.Pending())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an empty queue left its file")
	}
}

// A successful request (the network is back) sends the queue at once.
func TestScrobblerFlushesAfterSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), scrobbleQueueFile)
	s, _ := newTestScrobbler(t, path, time.Unix(1793000000, 0))
	api := &fakeAPI{err: errOffline}
	s.Configure([]*service{{name: svcListenBrainz, api: api, batch: 100}}, false)
	for i := range 3 {
		l := song1
		l.Started += int64(i * 300)
		s.Scrobble(l)
		s.Wait()
	}
	api.set(nil)
	s.NowPlaying(song2)
	s.Wait()
	if r, n := api.count(); r != 1 || n != 3 || s.Pending() != 0 {
		t.Fatalf("sent %d requests, %d listens; %d left", r, n, s.Pending())
	}
}

// Queued scrobbles go to Last.fm 50 at a time, and to ListenBrainz as an
// import.
func TestScrobblerBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), scrobbleQueueFile)
	now := time.Unix(1793000000, 0)
	var q queueFile
	for i := range 120 {
		l := song1
		l.Started = now.Unix() - int64(i*240)
		q.Listens = append(q.Listens, queuedListen{svcLastFM, l}, queuedListen{svcListenBrainz, l})
	}
	b, _ := json.Marshal(q)
	os.WriteFile(path, b, 0644)
	s, _ := newTestScrobbler(t, path, now)
	flm, lf := newFakeLastFM(t)
	flb, lb := newFakeLB(t)
	s.Configure([]*service{{name: svcLastFM, api: lf, batch: lastfmBatch}, {name: svcListenBrainz, api: lb, batch: listenBrainzBatch}}, false)
	s.Flush()
	s.Wait()
	var sizes []int
	for _, c := range flm.Calls() {
		n := 0
		for k := range c {
			if strings.HasPrefix(k, "artist[") {
				n++
			}
		}
		sizes = append(sizes, n)
	}
	if fmt.Sprint(sizes) != "[50 50 20]" {
		t.Errorf("Last.fm batches %v, want [50 50 20]", sizes)
	}
	var lbSizes []string
	flb.mu.Lock()
	defer flb.mu.Unlock()
	for _, p := range flb.payloads {
		lbSizes = append(lbSizes, fmt.Sprintf("%s:%d", p["listen_type"], len(p["payload"].([]any))))
	}
	if fmt.Sprint(lbSizes) != "[import:100 import:20]" {
		t.Errorf("ListenBrainz batches %v", lbSizes)
	}
	if s.Pending() != 0 {
		t.Errorf("%d left", s.Pending())
	}
}

// Last.fm ignores scrobbles over 14 days old, so they are dropped from the
// queue; ListenBrainz takes old listens, so those stay. The queue keeps at
// most maxQueued, dropping the oldest.
func TestScrobblerDropsOldAndOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), scrobbleQueueFile)
	now := time.Unix(1793000000, 0)
	old := song1
	old.Started = now.Add(-15 * 24 * time.Hour).Unix()
	recent := song2
	recent.Started = now.Add(-13 * 24 * time.Hour).Unix()
	q := queueFile{Listens: []queuedListen{{svcLastFM, old}, {svcListenBrainz, old}, {svcLastFM, recent}}}
	b, _ := json.Marshal(q)
	os.WriteFile(path, b, 0644)
	s, _ := newTestScrobbler(t, path, now)
	if got := fmt.Sprint(s.queue); got != fmt.Sprint([]queuedListen{{svcListenBrainz, old}, {svcLastFM, recent}}) {
		t.Errorf("kept %v", got)
	}

	s.mu.Lock()
	s.queue = nil
	for i := range maxQueued + 5 {
		l := song1
		l.Started = now.Unix() + int64(i)
		s.enqueue(svcListenBrainz, l)
	}
	n, first := len(s.queue), s.queue[0].Started
	s.mu.Unlock()
	if n != maxQueued || first != now.Unix()+5 {
		t.Errorf("%d queued, oldest %d", n, first-now.Unix())
	}
}

// A refused token or session isn't retried: the listens wait, and Settings
// shows the service needs a new login. A new login (fresh Configure) tries
// again.
func TestScrobblerNeedsLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), scrobbleQueueFile)
	s, clk := newTestScrobbler(t, path, time.Unix(1793000000, 0))
	api := &fakeAPI{err: &sendError{kind: errLogin, msg: "test: bad session"}}
	svcs := func() []*service { return []*service{{name: svcLastFM, api: api, batch: lastfmBatch}} }
	s.Configure(svcs(), false)
	s.Scrobble(song1)
	s.Wait()
	q := readQueueFile(path)
	if len(q.Listens) != 1 || fmt.Sprint(q.NeedsLogin) != "[lastfm]" {
		t.Fatalf("queue file %+v", q)
	}
	api.set(nil)
	clk.add(time.Hour)
	s.Tick(true)
	s.NowPlaying(song2)
	s.Scrobble(song2)
	s.Wait()
	if r, _ := api.count(); r != 0 || api.nowPlayings() != 0 {
		t.Fatal("sent with a refused login")
	}
	if s.Pending() != 2 {
		t.Fatalf("%d queued", s.Pending())
	}
	// The status screen reads it from the file.
	dir := filepath.Dir(path)
	st := readScrobbleStatus(dir, &Settings{}, map[string]string{secLFMKey: "k", secLFMSecret: "s", secLFMSession: "sk"})
	if !st.LFMBroken || st.Waiting != 2 || st.summary() != "Last.fm: log in again" {
		t.Errorf("status %+v", st)
	}

	s.Configure(svcs(), true) // logged in again
	s.Tick(true)
	s.Wait()
	if _, n := api.count(); n != 2 || s.Pending() != 0 {
		t.Fatalf("sent %d after logging in again; %d left", n, s.Pending())
	}

	// Turning a service off drops what waits for it.
	api.set(errOffline)
	s.Scrobble(song1)
	s.Wait()
	s.Configure(nil, true)
	if s.Pending() != 0 {
		t.Errorf("%d left for a service turned off", s.Pending())
	}
}

// After a 429, nothing is sent until Retry-After has passed, not even with
// Send now.
func TestScrobblerRetryAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), scrobbleQueueFile)
	s, clk := newTestScrobbler(t, path, time.Unix(1793000000, 0))
	api := &fakeAPI{err: &sendError{kind: errTemporary, retryAfter: 10 * time.Minute, msg: "test: 429"}}
	s.Configure([]*service{{name: svcListenBrainz, api: api, batch: 100}}, false)
	s.Scrobble(song1)
	s.Wait()
	api.set(nil)
	s.Scrobble(song2) // held: straight to the queue
	s.NowPlaying(song2)
	s.Flush()
	clk.add(9 * time.Minute)
	s.Tick(true)
	s.Wait()
	if r, _ := api.count(); r != 0 || api.nowPlayings() != 0 || s.Pending() != 2 {
		t.Fatalf("%d requests, %d now playing, %d queued during the hold", r, api.nowPlayings(), s.Pending())
	}
	clk.add(time.Minute)
	s.Tick(true)
	s.Wait()
	if _, n := api.count(); n != 2 || s.Pending() != 0 {
		t.Fatalf("after the hold: %d sent, %d left", n, s.Pending())
	}
}

// A listen the service refuses is dropped, not retried, and doesn't take
// the rest of its batch with it.
func TestScrobblerDropsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), scrobbleQueueFile)
	s, _ := newTestScrobbler(t, path, time.Unix(1793000000, 0))
	bad := Listen{Artist: "", Track: "?", Started: 1792999000}
	api := &fakeAPI{refuse: func(ls []Listen) error {
		for _, l := range ls {
			if l == bad {
				return &sendError{kind: errRejected, msg: "test: bad listen"}
			}
		}
		return nil
	}}
	s.mu.Lock()
	s.queue = []queuedListen{{svcListenBrainz, song1}, {svcListenBrainz, bad}, {svcListenBrainz, song2}}
	s.mu.Unlock()
	s.Configure([]*service{{name: svcListenBrainz, api: api, batch: 100}}, false)
	s.Flush()
	s.Wait()
	if _, n := api.count(); n != 2 || s.Pending() != 0 {
		t.Fatalf("sent %d, %d left", n, s.Pending())
	}
	s.Scrobble(bad)
	s.Wait()
	if s.Pending() != 0 {
		t.Error("a refused live listen was queued")
	}
}

// The calls return at once, whatever the network does: the daemon's loop
// never waits on a scrobble.
func TestScrobblerNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	s, _ := newTestScrobbler(t, filepath.Join(t.TempDir(), scrobbleQueueFile), time.Unix(1793000000, 0))
	lb := &ListenBrainz{Base: srv.URL, Token: "t", HTTP: srv.Client()}
	s.Configure([]*service{{name: svcListenBrainz, api: lb, batch: 100}}, false)
	start := time.Now()
	s.NowPlaying(song1)
	s.Scrobble(song1)
	s.Flush()
	s.Tick(true)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("took %v", d)
	}
	close(release)
	s.Wait()
}

// ---- scrobble.txt ----

func TestImportFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINIDECK_DEVKEY", filepath.Join(dir, "key"))
	path := filepath.Join(dir, importFile)
	secPath := filepath.Join(dir, "secrets.enc")
	lfKey, lfSecret := "0123456789abcdef0123456789ABCDEF", "fedcba9876543210fedcba9876543210"
	text := "\xef\xbb\xbf# from my computer\r\n" +
		"listenbrainz_token = 6a2e1c4f-8b0d-4e57-9a3c-2f1b7d6e5c40\r\n" +
		"lastfm_api_key=" + lfKey + "\r\n\r\n" +
		"lastfm_shared_secret=\"" + lfSecret + "\"\r\n"
	os.WriteFile(path, []byte(text), 0644)
	vals, err := readImportFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sec := map[string]string{"navidrome": "pw", secLFMSession: "old-session"}
	notDeleted, err := sealImport(path, secPath, sec, vals)
	if err != nil || notDeleted != nil {
		t.Fatal(err, notDeleted)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("scrobble.txt is still there")
	}
	got, err := secrets.Load(secPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"navidrome": "pw", secLBToken: "6a2e1c4f-8b0d-4e57-9a3c-2f1b7d6e5c40",
		secLFMKey: lfKey, secLFMSecret: lfSecret}
	if fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(sec) != fmt.Sprint(want) {
		t.Errorf("sealed %v\nscreen has %v\nwant %v (the old session goes with new keys)", got, sec, want)
	}
	raw, _ := os.ReadFile(secPath)
	if strings.Contains(string(raw), lfSecret) {
		t.Error("secrets.enc holds the secret in plain text")
	}

	// Files that aren't right are refused and left on the card, and the
	// errors never repeat the values.
	for _, bad := range []string{
		"lastfm_api_key=" + lfKey + "\n",
		"lastfm_api_key=" + lfKey + "\nlastfm_secret=" + lfSecret[:31] + "\n",
		"listenbrainz_token=short\n",
		"lastfm_password=hunter2hunter2\n",
		"just a token on its own\n",
		"# nothing\n",
	} {
		os.WriteFile(path, []byte(bad), 0644)
		_, err := readImportFile(path)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		for _, v := range []string{lfKey, lfSecret[:31], "short", "hunter2"} {
			if strings.Contains(err.Error(), v) {
				t.Errorf("%q: error shows a value: %v", bad, err)
			}
		}
		if _, err := os.Stat(path); err != nil {
			t.Error("a refused file was removed")
		}
	}

	// Saving fails (no device key can be made): nothing changes, the file
	// stays.
	t.Setenv("MINIDECK_DEVKEY", filepath.Join(dir, "no", "such", "key"))
	os.WriteFile(path, []byte("listenbrainz_token=6a2e1c4f-8b0d-4e57-9a3c-2f1b7d6e5c40\n"), 0644)
	before := fmt.Sprint(sec)
	if _, err := sealImport(path, secPath, sec, map[string]string{secLBToken: "new-token-0123456789abcdef"}); err == nil {
		t.Error("no error")
	}
	if fmt.Sprint(sec) != before {
		t.Error("the screen's secrets changed after a failed save")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the file went although nothing was saved")
	}
}

// The services the daemon builds from the saved logins.
func TestScrobbleServices(t *testing.T) {
	names := func(sec map[string]string) string {
		var n []string
		for _, s := range scrobbleServices(sec, http.DefaultClient) {
			n = append(n, s.name)
		}
		return strings.Join(n, ",")
	}
	if got := names(map[string]string{secLBToken: "t", secLFMKey: "k", secLFMSecret: "s"}); got != "listenbrainz" {
		t.Errorf("without a Last.fm session: %s", got)
	}
	if got := names(map[string]string{secLFMKey: "k", secLFMSecret: "s", secLFMSession: "sk"}); got != "lastfm" {
		t.Errorf("Last.fm: %s", got)
	}
}
