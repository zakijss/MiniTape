package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/zakijss/minideck/version"
)

// ListenBrainz is a client for ListenBrainz's listen API. It logs in with
// the user token from listenbrainz.org/settings.
type ListenBrainz struct {
	Base  string // https://api.listenbrainz.org (tests change it)
	Token string
	HTTP  *http.Client
}

const (
	listenBrainzAPI = "https://api.listenbrainz.org"
	// listenBrainzBatch keeps each import small (the API takes up to 1000).
	listenBrainzBatch = 100
)

func newListenBrainz(token string, c *http.Client) *ListenBrainz {
	return &ListenBrainz{Base: listenBrainzAPI, Token: token, HTTP: c}
}

func userAgent() string { return "MiniTape/" + version.Version + " (MiniDeck)" }

func (c *ListenBrainz) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	base := c.Base
	if base == "" {
		base = listenBrainzAPI
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.Token)
	req.Header.Set("User-Agent", userAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return netError("ListenBrainz", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return netError("ListenBrainz", err)
	}
	if resp.StatusCode != http.StatusOK {
		e := httpError("ListenBrainz", resp)
		// Errors come as {"code": 400, "error": "..."}; the message helps
		// in the log when a listen is refused.
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &msg) == nil && msg.Error != "" {
			m := msg.Error
			if len(m) > 120 {
				m = m[:120]
			}
			e.msg += ": " + m
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return &sendError{kind: errTemporary, msg: "ListenBrainz: unexpected reply"}
		}
	}
	return nil
}

// Validate checks the token and returns the user's name. A token
// ListenBrainz doesn't know gives an errLogin error.
func (c *ListenBrainz) Validate(ctx context.Context) (string, error) {
	var r struct {
		Valid    bool   `json:"valid"`
		UserName string `json:"user_name"`
	}
	if err := c.do(ctx, http.MethodGet, "/1/validate-token", nil, &r); err != nil {
		return "", err
	}
	if !r.Valid {
		return "", &sendError{kind: errLogin, msg: "ListenBrainz doesn't know that token."}
	}
	return r.UserName, nil
}

// lbListen is one listen in a submit-listens payload.
type lbListen struct {
	ListenedAt int64 `json:"listened_at,omitempty"`
	Track      struct {
		Artist  string         `json:"artist_name"`
		Track   string         `json:"track_name"`
		Release string         `json:"release_name,omitempty"`
		Info    map[string]any `json:"additional_info"`
	} `json:"track_metadata"`
}

func lbListenOf(l Listen, at int64) lbListen {
	var x lbListen
	x.ListenedAt = at
	x.Track.Artist, x.Track.Track, x.Track.Release = l.Artist, l.Track, l.Album
	x.Track.Info = map[string]any{
		"media_player":              "MiniTape",
		"submission_client":         "MiniTape",
		"submission_client_version": version.Version,
	}
	if l.Duration > 0 {
		x.Track.Info["duration_ms"] = l.Duration * 1000
	}
	if l.Number > 0 {
		x.Track.Info["tracknumber"] = l.Number
	}
	return x
}

func (c *ListenBrainz) submit(ctx context.Context, kind string, ls []lbListen) error {
	return c.do(ctx, http.MethodPost, "/1/submit-listens", map[string]any{"listen_type": kind, "payload": ls}, nil)
}

// NowPlaying submits a "playing_now" listen (no time: it is now).
func (c *ListenBrainz) NowPlaying(ctx context.Context, l Listen) error {
	return c.submit(ctx, "playing_now", []lbListen{lbListenOf(l, 0)})
}

// Submit sends listens: one as it is played is a "single"; queued ones go
// as an "import", the type ListenBrainz asks for listens sent late.
func (c *ListenBrainz) Submit(ctx context.Context, ls []Listen, queued bool) error {
	payload := make([]lbListen, len(ls))
	for i, l := range ls {
		payload[i] = lbListenOf(l, l.Started)
	}
	kind := "import"
	if !queued && len(ls) == 1 {
		kind = "single"
	}
	return c.submit(ctx, kind, payload)
}
