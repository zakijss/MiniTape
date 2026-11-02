package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/zakijss/minideck/atomicfile"
	"github.com/zakijss/minideck/tnet"
)

// Scrobbling to Last.fm and ListenBrainz happens in the player daemon, next
// to the music server's own scrobble: "now playing" when a song starts, a
// listen once half of it (or 4 minutes) has played. A listen that can't be
// sent (no Wi-Fi, the service is down or busy) waits in a queue file in the
// app's folder and is sent later.

// Listen is one play of a song, as the scrobbling services want it.
type Listen struct {
	Artist   string `json:"artist"`
	Track    string `json:"track"`
	Album    string `json:"album,omitempty"`
	Duration int    `json:"duration,omitempty"` // seconds
	Number   int    `json:"number,omitempty"`   // track number
	Started  int64  `json:"started"`            // Unix time the song started
}

func listenOf(s Song, started time.Time) Listen {
	return Listen{Artist: s.Artist, Track: s.Title, Album: s.Album, Duration: s.Duration,
		Number: s.Track, Started: started.Unix()}
}

// The services, by the names used in the queue file.
const (
	svcLastFM       = "lastfm"
	svcListenBrainz = "listenbrainz"
)

// Last.fm ignores scrobbles more than 14 days old, so there is no point
// keeping them in the queue. ListenBrainz takes listens of any age.
var maxAge = map[string]time.Duration{svcLastFM: 14 * 24 * time.Hour}

const (
	scrobbleQueueFile = "scrobble-queue.json"
	// maxQueued caps the queue: about two weeks of listening all day. The
	// oldest go first.
	maxQueued = 2000
	// While music plays, the queue is tried again after a failure, first
	// after 2 minutes and then less often, up to every 16 minutes.
	firstBackoff = 2 * time.Minute
	maxBackoff   = 16 * time.Minute
	// sendTimeout bounds each request; the HTTP client has its own
	// timeouts too.
	sendTimeout = 30 * time.Second
)

// errKind says what a failed request means for the listens in it.
type errKind int

const (
	// errTemporary: no network, a server error or a rate limit. Keep the
	// listens and try again later.
	errTemporary errKind = iota
	// errRejected: the service refused these listens (bad data). Sending
	// them again would fail the same way, so they are dropped.
	errRejected
	// errLogin: the token, API key or session is no good. Keep the
	// listens, stop trying, and ask the user to log in again.
	errLogin
)

// sendError is a failed request to a scrobbling service. Its message is
// safe to log and to show: it never holds a token, key or signature.
type sendError struct {
	kind       errKind
	code       int           // the service's error code, if it gave one
	retryAfter time.Duration // from a 429's Retry-After
	msg        string
}

func (e *sendError) Error() string { return e.msg }

// kindOf is how an error from a service should be handled. Errors that
// aren't a sendError come from the network: temporary.
func kindOf(err error) errKind {
	var se *sendError
	if errors.As(err, &se) {
		return se.kind
	}
	return errTemporary
}

func retryAfterOf(err error) time.Duration {
	var se *sendError
	if errors.As(err, &se) {
		return se.retryAfter
	}
	return 0
}

// netError describes a failed connection without the request's URL.
func netError(service string, err error) error {
	return &sendError{kind: errTemporary, msg: service + ": " + tnet.Explain(err).Error()}
}

// httpError classifies a reply that isn't a success by its status alone,
// for replies the service didn't explain.
func httpError(service string, resp *http.Response) *sendError {
	e := &sendError{kind: errTemporary, msg: service + ": " + resp.Status}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e.retryAfter = parseRetryAfter(resp.Header)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		e.kind = errLogin
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout:
		e.kind = errRejected
	}
	return e
}

// parseRetryAfter reads Retry-After (seconds or a date), or ListenBrainz's
// X-RateLimit-Reset-In, capped at an hour so a wrong value can't stop
// scrobbling for days. Without either it gives a minute.
func parseRetryAfter(h http.Header) time.Duration {
	d := time.Minute
	if v := h.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			d = time.Duration(n) * time.Second
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
		}
	} else if v := h.Get("X-RateLimit-Reset-In"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			d = time.Duration(n) * time.Second
		}
	}
	return min(max(d, time.Second), time.Hour)
}

// scrobbleAPI is a scrobbling service's client.
type scrobbleAPI interface {
	NowPlaying(ctx context.Context, l Listen) error
	// Submit sends listens; queued marks listens sent late from the queue
	// (ListenBrainz files them as an import).
	Submit(ctx context.Context, ls []Listen, queued bool) error
}

// service is one scrobbling service the daemon sends to, with its retry
// state. The state is guarded by Scrobbler.mu.
type service struct {
	name  string
	api   scrobbleAPI
	batch int // most listens per request

	holdUntil time.Time     // after a 429: no requests before this
	retryAt   time.Time     // the queue's next try
	backoff   time.Duration // the wait after the last failure
	flushing  bool          // sending from the queue now
}

// queuedListen is a listen waiting to be sent to one service.
type queuedListen struct {
	Service string `json:"service"`
	Listen
}

// queueFile is scrobble-queue.json. The screen reads it to show how many
// listens are waiting and which service needs a new login.
type queueFile struct {
	NeedsLogin []string       `json:"needs_login,omitempty"`
	Listens    []queuedListen `json:"listens"`
}

// readQueueFile reads the queue file; a missing or damaged one is empty.
func readQueueFile(path string) queueFile {
	var q queueFile
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &q); err != nil {
			log.Printf("%s: %v", path, err)
			return queueFile{}
		}
	}
	return q
}

// Scrobbler sends listens to the configured services and keeps the ones
// that couldn't be sent. Its methods return at once: requests run in
// goroutines, so the daemon's loop (and the music) never waits on the
// network.
type Scrobbler struct {
	path string
	now  func() time.Time

	mu         sync.Mutex
	svcs       map[string]*service
	queue      []queuedListen // oldest first
	needsLogin map[string]bool

	saveMu sync.Mutex     // one write of the queue file at a time
	wg     sync.WaitGroup // requests in flight
}

// NewScrobbler reads the queue left at path by an earlier run.
func NewScrobbler(path string) *Scrobbler { return newScrobbler(path, time.Now) }

func newScrobbler(path string, now func() time.Time) *Scrobbler {
	s := &Scrobbler{path: path, now: now, svcs: map[string]*service{}, needsLogin: map[string]bool{}}
	q := readQueueFile(path)
	s.queue = q.Listens
	for _, n := range q.NeedsLogin {
		s.needsLogin[n] = true
	}
	s.prune()
	return s
}

// Configure sets the services to send to. fresh means the user has just
// changed the logins: services marked as needing a new login get another
// try, and listens queued for a service that has been turned off are
// dropped. The first call after startup isn't fresh, so it keeps both.
func (s *Scrobbler) Configure(svcs []*service, fresh bool) {
	s.mu.Lock()
	s.svcs = map[string]*service{}
	for _, v := range svcs {
		s.svcs[v.name] = v
	}
	if fresh {
		s.needsLogin = map[string]bool{}
		keep := s.queue[:0]
		for _, q := range s.queue {
			if s.svcs[q.Service] != nil {
				keep = append(keep, q)
			}
		}
		s.queue = keep
	}
	s.mu.Unlock()
	if fresh {
		s.persist()
	}
}

// usable reports whether requests to v may be sent now. Called with mu
// held.
func (s *Scrobbler) usable(v *service) bool {
	return !s.needsLogin[v.name] && !s.now().Before(v.holdUntil)
}

// NowPlaying tells each service what has just started. It isn't queued: by
// the time it could be sent, something else is playing.
func (s *Scrobbler) NowPlaying(l Listen) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.svcs {
		if !s.usable(v) {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			s.done(v, v.api.NowPlaying(ctx, l), nil)
		}()
	}
}

// Scrobble sends a listen to each service, queueing it where that fails.
func (s *Scrobbler) Scrobble(l Listen) {
	s.mu.Lock()
	queued := false
	for _, v := range s.svcs {
		if !s.usable(v) {
			s.enqueue(v.name, l)
			queued = true
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			s.done(v, v.api.Submit(ctx, []Listen{l}, false), []Listen{l})
		}()
	}
	s.mu.Unlock()
	if queued {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.persist() }()
	}
}

// done handles the result of a request made as music played: on success
// the queue is tried at once (the network is back); a failed listen is
// queued, or dropped if the service refused it.
func (s *Scrobbler) done(v *service, err error, ls []Listen) {
	s.mu.Lock()
	changed := false
	switch {
	case err == nil:
		v.backoff, v.retryAt = 0, time.Time{}
		s.startFlush(v)
	case kindOf(err) == errRejected:
		if len(ls) > 0 {
			log.Printf("scrobble: dropped a listen %s refused: %v", v.name, err)
		} else {
			log.Printf("scrobble: %v", err)
		}
	default:
		log.Printf("scrobble: %v", err)
		for _, l := range ls {
			s.enqueue(v.name, l)
		}
		changed = len(ls) > 0
		changed = s.failed(v, err) || changed
	}
	s.mu.Unlock()
	if changed {
		s.persist()
	}
}

// failed records a failure: a bad login stops requests until the user logs
// in again; anything else waits a while, longer each time. It reports
// whether the queue file needs writing. Called with mu held.
func (s *Scrobbler) failed(v *service, err error) bool {
	if kindOf(err) == errLogin {
		if s.needsLogin[v.name] {
			return false
		}
		s.needsLogin[v.name] = true
		return true
	}
	now := s.now()
	v.backoff = min(max(2*v.backoff, firstBackoff), maxBackoff)
	v.retryAt = now.Add(v.backoff)
	if d := retryAfterOf(err); d > 0 {
		v.holdUntil = now.Add(d)
		if v.retryAt.Before(v.holdUntil) {
			v.retryAt = v.holdUntil
		}
	}
	return false
}

// enqueue adds a listen to the queue. Called with mu held.
func (s *Scrobbler) enqueue(name string, l Listen) {
	s.queue = append(s.queue, queuedListen{name, l})
	s.prune()
}

// prune drops listens too old for their service, then the oldest beyond
// maxQueued. Called with mu held.
func (s *Scrobbler) prune() {
	now := s.now()
	keep := s.queue[:0]
	for _, q := range s.queue {
		if age, ok := maxAge[q.Service]; ok && now.Sub(time.Unix(q.Started, 0)) > age {
			continue
		}
		keep = append(keep, q)
	}
	if over := len(keep) - maxQueued; over > 0 {
		keep = append(keep[:0], keep[over:]...)
	}
	s.queue = keep
}

// Tick runs once a second from the daemon's loop. While music plays (the
// device is awake and likely online), it tries the queue again when a
// service's wait is over.
func (s *Scrobbler) Tick(playing bool) {
	if !playing {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.svcs {
		if !s.now().Before(v.retryAt) {
			s.startFlush(v)
		}
	}
}

// Flush tries the queue now, without waiting for the back-off ("Send now"
// in Settings). A service's Retry-After and a bad login still hold.
func (s *Scrobbler) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.svcs {
		s.startFlush(v)
	}
}

// startFlush starts sending v's queued listens, if it has some and may
// send. Called with mu held.
func (s *Scrobbler) startFlush(v *service) {
	if v.flushing || !s.usable(v) || !s.hasQueued(v.name) {
		return
	}
	v.flushing = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.flush(v)
	}()
}

func (s *Scrobbler) hasQueued(name string) bool {
	for _, q := range s.queue {
		if q.Service == name {
			return true
		}
	}
	return false
}

// flush sends v's queued listens in batches, oldest first, until the queue
// is empty or a request fails.
func (s *Scrobbler) flush(v *service) {
	batch := max(v.batch, 1)
	for {
		s.mu.Lock()
		s.prune()
		var ls []Listen
		for _, q := range s.queue {
			if q.Service == v.name && len(ls) < batch {
				ls = append(ls, q.Listen)
			}
		}
		// Stop when the logins were changed meanwhile (Configure made a
		// new service): its own flush takes over, and two would send the
		// same listens twice.
		if len(ls) == 0 || !s.usable(v) || s.svcs[v.name] != v {
			v.flushing = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		err := v.api.Submit(ctx, ls, true)
		cancel()

		s.mu.Lock()
		stop := false
		switch {
		case err == nil:
			v.backoff, v.retryAt = 0, time.Time{}
			s.remove(v.name, ls)
		case kindOf(err) == errRejected && len(ls) > 1:
			// One bad listen spoils a batch: send the rest one at a time,
			// so only the bad one is dropped.
			batch = 1
		case kindOf(err) == errRejected:
			log.Printf("scrobble: dropped a queued listen %s refused: %v", v.name, err)
			s.remove(v.name, ls)
		default:
			log.Printf("scrobble: queue: %v", err)
			s.failed(v, err)
			v.flushing = false
			stop = true
		}
		s.mu.Unlock()
		s.persist()
		if stop {
			return
		}
	}
}

// remove takes sent listens out of the queue: the first match of each, as
// more may have been added meanwhile. Called with mu held.
func (s *Scrobbler) remove(name string, ls []Listen) {
	for _, l := range ls {
		for i, q := range s.queue {
			if q.Service == name && q.Listen == l {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				break
			}
		}
	}
}

// persist writes the queue file. The state is copied under saveMu, so the
// last write always holds the latest state.
func (s *Scrobbler) persist() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	q := queueFile{Listens: append([]queuedListen{}, s.queue...)}
	for n := range s.needsLogin {
		q.NeedsLogin = append(q.NeedsLogin, n)
	}
	s.mu.Unlock()
	if len(q.Listens) == 0 && len(q.NeedsLogin) == 0 {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			log.Printf("scrobble: %v", err)
		}
		return
	}
	b, err := json.Marshal(q)
	if err == nil {
		err = atomicfile.Write(s.path, b, 0644)
	}
	if err != nil {
		log.Printf("scrobble: saving the queue: %v", err)
	}
}

// Wait waits for the requests in flight (tests).
func (s *Scrobbler) Wait() { s.wg.Wait() }

// Pending is the number of queued listens.
func (s *Scrobbler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}
