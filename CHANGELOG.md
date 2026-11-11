# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Y refreshes a list from the server; lists still open at once from the copy on the card.
- Search, on the first screen: artists, albums and songs.
- X shuffles an album or playlist.
- Left and Right go back or forward 10 seconds in a song.
- R2 on Now Playing stars or unstars the song.
- L2 and R2 jump to the top and the bottom of a list.
- START plays an album from an artist's page.

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

[Unreleased]: https://github.com/zakijss/MiniTape/compare/v1.0.3...HEAD
[1.0.3]: https://github.com/zakijss/MiniTape/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/zakijss/MiniTape/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/zakijss/MiniTape/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniTape/releases/tag/v1.0.0
