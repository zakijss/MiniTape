# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [1.2.0] - 2026-11-19

### Added

- Music on the SD card: **On this card** on the first screen plays the songs in `Media/Music` (or `music_dir` in `settings.json`) by folder, artist or album, through the same player, with the sleep timer, background playback and the menu ticker. It works offline and with no server: the first start can skip the server. MP3, FLAC, Ogg, Opus, M4A, AAC and WAV.
- Tags, lengths and track numbers are read from the files (ID3, Vorbis comments, MP4), and covers from `cover.jpg`, `folder.jpg` or `front.jpg` in the folder. The index is kept on the card; Y reads the folder again.
- Lyrics for songs on the card, from a `.lrc` file next to the song or from its tags. Songs from the card go to Last.fm and ListenBrainz when they are tagged with an artist.
- Repeat: L2 on Now Playing goes through repeat all, repeat one and off.
- The queue, the song playing and the time in it are saved, and the next start opens on them, paused: A plays on. The place is written every 10 seconds while music plays, so a device that loses power picks up close to where it was. Server songs are dropped when another server (or none) is set up, and songs on the card when their files are gone.
- Down on Now Playing lists the whole queue with the current song marked. A plays a song, X takes one out, L2 and R2 move one up or down.
- Left and Right in Artists, and in Albums and Folders on the card, go to the first entry of the previous or next letter. Server artists follow the server's own letters.
- Covers inside the files on the card (ID3, FLAC and Ogg pictures, MP4 covr) when the folder has no cover picture. They are read when first shown, up to 8 MB each, and kept scaled in the cache.

### Changed

- A held Up or Down stops at the first or last row of a list instead of going round to the other end.

### Fixed

- A song played again (after Stop, or a one-song queue) is sent to Last.fm, ListenBrainz and the server as a new play. Before, it counted only once.
- In Search and Starred, song lengths and the playing mark showed on the wrong rows when albums were listed above the songs, and artists lost their icon.
- After switching servers, covers, lyrics and stars kept in memory from the old server are dropped, so another server's albums can't show them.
- Lyrics no longer crash the screen for a song the server lists without an id.
- A server can no longer make MiniTape read files on the card: songs from a server never count as files on the card, and covers and lyrics are read only from ordinary files inside the music folder, a cover at most 8 MB.
- Playing a folder queues at most 500 songs around the one chosen, instead of every song below it.
- A song that ends as soon as it starts (an empty or unreadable file) stops with an error instead of starting again and again with repeat on, each time counted as a play.
- A file with thousands of empty WAV chunks or MP4 boxes is read only so far, instead of keeping the card busy.
- The queue list ignores an older reading of the queue that arrives late, and the selection moves with a song only once the player has moved it.

## [1.1.1] - 2026-11-12

### Changed

- Reaches the WireGuard and NetBird networks MiniScale 1.2 can join, through its proxy (MiniDeck 1.1.0).

## [1.1.0] - 2026-11-12

### Added

- Y refreshes a list from the server; lists still open at once from the copy on the card.
- Search, on the first screen: artists, albums and songs.
- X shuffles an album or playlist.
- Left and Right go back or forward 10 seconds in a song.
- R2 on Now Playing stars or unstars the song.
- L2 and R2 jump to the top and the bottom of a list.
- START plays an album from an artist's page.
- A sleep timer: SELECT on Now Playing pauses the music after 15 to 90 minutes, or at the end of the song. It keeps counting with MiniTape closed.

### Fixed

- Holding a button no longer repeats it, except to scroll: a held A paused and played over and over.
- "1 song", not "1 songs".

### Changed

- Dependencies updated: golang.org/x/image 0.48.0, x/sys 0.50.0, x/text 0.44.0, newer root certificates; Go 1.27.4.

## [1.0.3] - 2026-11-08

### Changed

- A failed token check asks whether to keep the token anyway.
- Settings has the title "Music server", like its menu entry.
- Shuffle on an empty server says to add some music to it.
- "Can't start ffplay" starts with a capital.
- Grey and red small text is easier to read (MiniDeck 1.0.3).

## [1.0.2] - 2026-11-03

### Fixed

- A server sending a cover image with a huge declared size (for example 65535x65535 JPEG) could get past the size check on the device and run it out of memory; such covers are now refused.

## [1.0.1] - 2026-11-02

### Fixed

- The on-screen keyboard now shows how to leave it: B on an empty field goes back, and the footer shows B Back, or MENU Cancel once there is text (MiniDeck 1.0.1).

## [1.0.0] - 2026-11-02

### Added

- The first release: browsing, background playback with an on-screen ticker, lyrics, pixel-art covers, Last.fm and ListenBrainz scrobbling, songs downloaded ahead to the card with bit rate steps, and real-time audio scheduling. Settings for the streaming quality, the room for downloaded songs (and clearing them), the menu ticker, background playback and cover art.

[Unreleased]: https://github.com/zakijss/MiniTape/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/zakijss/MiniTape/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/zakijss/MiniTape/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/zakijss/MiniTape/compare/v1.0.3...v1.1.0
[1.0.3]: https://github.com/zakijss/MiniTape/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/zakijss/MiniTape/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/zakijss/MiniTape/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniTape/releases/tag/v1.0.0
