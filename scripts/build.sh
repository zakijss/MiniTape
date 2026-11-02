#!/bin/sh
# Builds the app for the device (ARMv7) into dist/, stamped with VERSION.
#   VERSION=1.0.0 ./scripts/build.sh
set -e
cd "$(dirname "$0")/.."
VERSION=${VERSION:-dev}
export CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-buildvcs=false
mkdir -p dist
ld="-s -w -X github.com/zakijss/minideck/version.stamp=minideck-version=$VERSION"
for c in minitape; do
    go build -trimpath -ldflags "$ld" -o "dist/$c" "./cmd/$c"
done
ls -l dist
