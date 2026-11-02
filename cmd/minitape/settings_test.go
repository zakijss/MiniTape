package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A settings.json from before these settings keeps today's behaviour.
func TestSettingsDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	old := `{"url":"http://homeserver:4533","user":"alex","secret":"navidrome","format":"mp3","max_bitrate":0}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := loadSettings(dir)
	if cfg.bitRate() != 192 || cfg.songCache() != 500<<20 || !cfg.menuTicker() {
		t.Fatalf("bit rate %d, cache %d, ticker %v", cfg.bitRate(), cfg.songCache(), cfg.menuTicker())
	}
	if it := qualityItem(cfg); it.Detail != "192 kbit/s" {
		t.Errorf("quality shows %q", it.Detail)
	}
	if it := songCacheItem(cfg); it.Detail != "500 MB" {
		t.Errorf("downloaded songs shows %q", it.Detail)
	}
}

// The new settings are written to settings.json and read back.
func TestSettingsSaved(t *testing.T) {
	dir := t.TempDir()
	cfg := loadSettings(dir)
	for _, label := range []string{"Streaming quality", "Downloaded songs", "Menu ticker"} {
		if !stepSetting(&cfg, label, 1) {
			t.Fatalf("%s isn't a value row", label)
		}
	}
	if err := saveSettings(dir, &cfg); err != nil {
		t.Fatal(err)
	}
	got := loadSettings(dir)
	if got.bitRate() != 256 || got.songCache() != 1024<<20 || got.menuTicker() {
		t.Fatalf("read back bit rate %d, cache %d, ticker %v", got.bitRate(), got.songCache(), got.menuTicker())
	}
	if it := songCacheItem(got); it.Detail != "1 GB" {
		t.Errorf("downloaded songs shows %q", it.Detail)
	}
	if it := menuTickerItem(got); it.Detail != "Off" {
		t.Errorf("menu ticker shows %q", it.Detail)
	}
}

func TestStepChoices(t *testing.T) {
	for _, c := range []struct{ cur, n, want int }{
		{192, 1, 256},
		{192, -1, 128},
		{320, 1, 96}, // wraps around
		{96, -1, 320},
		{160, 1, 192}, // set by hand: the neighbour in that direction
		{160, -1, 128},
		{200, 1, 256},
		{200, -1, 192},
		{1000, 1, 96},
	} {
		if got := step(bitRates, c.cur, c.n); got != c.want {
			t.Errorf("step(%d, %d) = %d, want %d", c.cur, c.n, got, c.want)
		}
	}
	if got := step(songCaches, 500, 1); got != 1024 {
		t.Errorf("after 500 MB comes %d", got)
	}
}

// Rows that open a screen or ask first aren't stepped.
func TestStepSettingRows(t *testing.T) {
	var cfg Settings
	for _, label := range []string{"Music server", "Scrobbling", "Clear downloaded songs", "Appearance"} {
		if stepSetting(&cfg, label, 1) {
			t.Errorf("%s stepped", label)
		}
	}
	stepSetting(&cfg, "Streaming quality", -1)
	if cfg.MaxBitRate != 128 {
		t.Errorf("left from the default gave %d", cfg.MaxBitRate)
	}
	stepSetting(&cfg, "Cover art", -1)
	if cfg.pixelCovers() {
		t.Error("cover art didn't switch")
	}
	stepSetting(&cfg, "Background playback", 1)
	if cfg.background() {
		t.Error("background playback didn't switch")
	}
}

// The relay asks for the setting first, then steps down from it.
func TestRelayRatesFollowSetting(t *testing.T) {
	for top, want := range map[int][]int{
		320: {320, 192, 128, 96},
		256: {256, 192, 128, 96},
		192: {192, 128, 96},
		128: {128, 96},
		96:  {96},
	} {
		r := &relay{maxRate: func() int { return top }}
		if got := r.rates(); !slices.Equal(got, want) {
			t.Errorf("at %d: rates %v, want %v", top, got, want)
		}
	}
}

// Downloaded songs beyond the limit are dropped, the least recently played
// first.
func TestRelayKeepsSongsWithinLimit(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"old", "new"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, 1<<20), 0644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(i-2) * time.Hour)
		os.Chtimes(p, at, at)
	}
	r, err := startRelay(func(string, int, int) string { return "" }, func() int { return 192 },
		func() string { return "mp3" }, dir, func() int64 { return 1 << 20 })
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	deadline := time.Now().Add(5 * time.Second)
	for dirSize(dir) > 1<<20 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
		t.Error("the older song is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err != nil {
		t.Error("the newer song was dropped")
	}
}

func TestClearSongsRow(t *testing.T) {
	dir := t.TempDir()
	if it := clearSongsItem(dirSize(dir)); it.Sub != "None on the card" {
		t.Errorf("empty: %q", it.Sub)
	}
	os.WriteFile(filepath.Join(dir, "a"), make([]byte, 3<<20), 0644)
	if it := clearSongsItem(dirSize(dir)); it.Sub != "3 MB on the card now" {
		t.Errorf("3 MB: %q", it.Sub)
	}
	if dirSize(filepath.Join(dir, "missing")) != 0 {
		t.Error("a missing folder has a size")
	}
	for n, want := range map[int64]string{0: "0 MB", 1000: "Under 1 MB", 250 << 20: "250 MB", 1 << 30: "1 GB", 3 << 29: "1.5 GB"} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}
