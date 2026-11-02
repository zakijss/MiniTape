package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// LastFM is a client for Last.fm's scrobbling API (version 2.0). There is
// no API key shipped with MiniTape: each user makes their own at
// last.fm/api/account/create and copies the key and shared secret to the
// device. Logging in then gives a session key, which doesn't expire.
type LastFM struct {
	Base    string // https://ws.audioscrobbler.com/2.0/ (tests change it)
	Key     string // API key
	Secret  string // shared secret: signs requests, never sent
	Session string // session key, after logging in
	HTTP    *http.Client
}

const (
	lastfmAPI = "https://ws.audioscrobbler.com/2.0/"
	// lastfmBatch is the most scrobbles track.scrobble takes at once.
	lastfmBatch = 50
)

// Last.fm's error codes that matter here.
const (
	lfmInvalidParams  = 6
	lfmAuthFailed     = 4
	lfmOperationFail  = 8
	lfmInvalidSession = 9
	lfmInvalidKey     = 10
	lfmOffline        = 11
	lfmBadSignature   = 13
	lfmNotApproved    = 14 // the user hasn't approved the token yet
	lfmTokenExpired   = 15
	lfmTemporary      = 16
	lfmKeySuspended   = 26
	lfmRateLimited    = 29
)

func newLastFM(key, secret, session string, c *http.Client) *LastFM {
	return &LastFM{Base: lastfmAPI, Key: key, Secret: secret, Session: session, HTTP: c}
}

// lastfmSign computes api_sig: the parameters sorted by name, each name
// followed by its value, then the shared secret, MD5'd. format and
// callback aren't signed.
func lastfmSign(params url.Values, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "format" && k != "callback" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(params.Get(k))
	}
	b.WriteString(secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// lastfmAuthURL is the page where the user approves a token. It is the
// shortest form Last.fm accepts (last.fm redirects to www.last.fm with the
// query kept), so it fits the largest QR code the encoder makes.
func lastfmAuthURL(key, token string) string {
	return "https://last.fm/api/auth/?api_key=" + url.QueryEscape(key) + "&token=" + url.QueryEscape(token)
}

// call sends a signed POST and decodes the JSON reply into out.
func (c *LastFM) call(ctx context.Context, method string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("method", method)
	params.Set("api_key", c.Key)
	params.Set("api_sig", lastfmSign(params, c.Secret))
	params.Set("format", "json")
	base := c.Base
	if base == "" {
		base = lastfmAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return netError("Last.fm", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return netError("Last.fm", err)
	}
	// Errors come as {"error": 9, "message": "..."}, with or without an
	// HTTP error status.
	var e struct {
		Error   int    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != 0 {
		se := lastfmError(e.Error, e.Message)
		if resp.StatusCode == http.StatusTooManyRequests {
			se.retryAfter = parseRetryAfter(resp.Header)
		}
		return se
	}
	if resp.StatusCode != http.StatusOK {
		return httpError("Last.fm", resp)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return &sendError{kind: errTemporary, msg: "Last.fm: unexpected reply"}
		}
	}
	return nil
}

// lastfmError sorts Last.fm's error codes into what to do about them.
func lastfmError(code int, msg string) *sendError {
	e := &sendError{kind: errRejected, code: code}
	switch code {
	case lfmInvalidSession, lfmAuthFailed, lfmInvalidKey, lfmBadSignature, lfmKeySuspended,
		lfmNotApproved, lfmTokenExpired:
		e.kind = errLogin
	case lfmOperationFail, lfmOffline, lfmTemporary:
		e.kind = errTemporary
	case lfmRateLimited:
		e.kind, e.retryAfter = errTemporary, 5*time.Minute
	}
	if len(msg) > 120 {
		msg = msg[:120]
	}
	e.msg = fmt.Sprintf("Last.fm: %s (error %d)", strings.TrimSpace(msg), code)
	return e
}

// Token starts a login: Last.fm gives a token, which the user approves on
// the web (lastfmAuthURL) within an hour.
func (c *LastFM) Token(ctx context.Context) (string, error) {
	var r struct {
		Token string `json:"token"`
	}
	if err := c.call(ctx, "auth.getToken", nil, &r); err != nil {
		return "", err
	}
	if r.Token == "" {
		return "", &sendError{kind: errTemporary, msg: "Last.fm: no token in the reply"}
	}
	return r.Token, nil
}

// GetSession trades an approved token for a session key and the user's
// name. Until the user approves it, it fails with lfmNotApproved.
func (c *LastFM) GetSession(ctx context.Context, token string) (name, key string, err error) {
	var r struct {
		Session struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"session"`
	}
	if err := c.call(ctx, "auth.getSession", url.Values{"token": {token}}, &r); err != nil {
		return "", "", err
	}
	if r.Session.Key == "" {
		return "", "", &sendError{kind: errTemporary, msg: "Last.fm: no session in the reply"}
	}
	return r.Session.Name, r.Session.Key, nil
}

// pollSession asks for the session every interval until the user approves
// the token, the token expires or ctx ends. Network trouble in between
// doesn't end it: the phone may be on the same weak Wi-Fi.
func (c *LastFM) pollSession(ctx context.Context, token string, every time.Duration) (name, key string, err error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		name, key, err = c.GetSession(rctx, token)
		cancel()
		if err == nil {
			return name, key, nil
		}
		var se *sendError
		if errors.As(err, &se) && se.code != lfmNotApproved && se.kind != errTemporary {
			return "", "", err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-t.C:
		}
	}
}

// NowPlaying is track.updateNowPlaying.
func (c *LastFM) NowPlaying(ctx context.Context, l Listen) error {
	p := url.Values{"artist": {l.Artist}, "track": {l.Track}, "sk": {c.Session}}
	if l.Album != "" {
		p.Set("album", l.Album)
	}
	if l.Duration > 0 {
		p.Set("duration", fmt.Sprint(l.Duration))
	}
	if l.Number > 0 {
		p.Set("trackNumber", fmt.Sprint(l.Number))
	}
	return c.call(ctx, "track.updateNowPlaying", p, nil)
}

// Submit is track.scrobble, for up to lastfmBatch listens. Scrobbles
// Last.fm accepts but ignores (too old, a filtered artist) count as sent:
// sending them again wouldn't change that.
func (c *LastFM) Submit(ctx context.Context, ls []Listen, queued bool) error {
	p := url.Values{"sk": {c.Session}}
	for i, l := range ls {
		n := func(k string) string { return fmt.Sprintf("%s[%d]", k, i) }
		p.Set(n("artist"), l.Artist)
		p.Set(n("track"), l.Track)
		p.Set(n("timestamp"), fmt.Sprint(l.Started))
		if l.Album != "" {
			p.Set(n("album"), l.Album)
		}
		if l.Duration > 0 {
			p.Set(n("duration"), fmt.Sprint(l.Duration))
		}
		if l.Number > 0 {
			p.Set(n("trackNumber"), fmt.Sprint(l.Number))
		}
	}
	return c.call(ctx, "track.scrobble", p, nil)
}
