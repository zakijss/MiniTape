package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zakijss/minideck/cache"
)

func TestCachedLists(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		name := "Old"
		if hits.Load() > 1 {
			name = "New"
		}
		w.Write([]byte(`{"subsonic-response":{"status":"ok","albumList2":{"album":[{"id":"1","name":"` + name + `"}]}}}`))
	}))
	c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client(), Cache: cache.New(t.TempDir())}

	al, err := c.AlbumList("newest", 10)
	if err != nil || len(al) != 1 || al[0].Name != "Old" || hits.Load() != 1 {
		t.Fatalf("first: %v %v hits=%d", al, err, hits.Load())
	}
	// second time: the saved list at once, refreshed in the background
	al, err = c.AlbumList("newest", 10)
	if err != nil || al[0].Name != "Old" {
		t.Fatalf("second: %v %v", al, err)
	}
	for i := 0; hits.Load() < 2 && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the refresh write the cache
	al, _ = c.AlbumList("newest", 10)
	if al[0].Name != "New" {
		t.Fatalf("after refresh: %v", al)
	}
	// offline: still there
	srv.Close()
	if al, err = c.AlbumList("newest", 10); err != nil || len(al) != 1 {
		t.Fatalf("offline: %v %v", al, err)
	}
	// random lists are never cached: with the server gone, they fail
	if _, err := c.AlbumList("random", 10); err == nil {
		t.Fatalf("random list served from cache")
	}
}

func TestCachedCover(t *testing.T) {
	var hits atomic.Int32
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(buf.Bytes())
	}))
	dir := t.TempDir()
	c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client(), Cache: cache.New(dir)}
	if _, err := c.CoverArt("al-1", 240); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("cover not saved: %d files", len(ents))
	}
	srv.Close()
	if _, err := c.CoverArt("al-1", 240); err != nil || hits.Load() != 1 {
		t.Fatalf("cover not read from the card: %v hits=%d", err, hits.Load())
	}
}

// TestCoverSizes checks that covers come back at the size asked for,
// centre-cropped, with one card entry per size, and that a huge original
// is refused before it is decoded.
func TestCoverSizes(t *testing.T) {
	// 600x400: red and blue 100 px bands at the sides, green between
	src := image.NewRGBA(image.Rect(0, 0, 600, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 600; x++ {
			c := color.RGBA{0, 200, 0, 255}
			if x < 100 {
				c = color.RGBA{200, 0, 0, 255}
			} else if x >= 500 {
				c = color.RGBA{0, 0, 200, 255}
			}
			src.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, src)
	var sizes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sizes = append(sizes, r.URL.Query().Get("size"))
		w.Write(buf.Bytes())
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client(), Cache: cache.New(dir)}
	for _, px := range []int{thumbPx, largePx} {
		img, err := c.CoverArt("al-1", px)
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != px || b.Dy() != px {
			t.Fatalf("size %d: got %v", px, b)
		}
		// the side bands were cropped away: the edges are green
		for _, x := range []int{0, px - 1} {
			if r, g, _, _ := img.At(x, px/2).RGBA(); r>>8 > 20 || g>>8 < 180 {
				t.Errorf("size %d: edge pixel %d not cropped: %v", px, x, img.At(x, px/2))
			}
		}
	}
	if len(sizes) != 2 || sizes[0] != fmt.Sprint(thumbPx) || sizes[1] != fmt.Sprint(largePx) {
		t.Errorf("sizes asked for: %v", sizes)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Errorf("want one card entry per size, got %d", len(ents))
	}

	// a 2600x2600 original (6.8 MP) is refused from its header alone
	huge := image.NewGray(image.Rect(0, 0, 2600, 2600))
	buf.Reset()
	png.Encode(&buf, huge)
	if _, err := decodeCover(buf.Bytes(), largePx); err == nil {
		t.Error("huge cover decoded")
	}
}
