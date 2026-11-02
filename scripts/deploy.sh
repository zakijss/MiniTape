#!/bin/sh
# Builds the app and copies its binaries to a Miyoo over SSH, for trying a
# change without making a release. Settings and data in the app's folder
# are left alone. The device needs an SSH server that runs sh, cat and mv
# (MiniScale's Tailscale SSH works).
#   ./scripts/deploy.sh [USER@HOST]      (default root@miyoo)
set -e
cd "$(dirname "$0")/.."
host=${1:-root@miyoo}
VERSION=dev ./scripts/build.sh >/dev/null
dir=/mnt/SDCARD/App/MiniTape
put() { # local file, remote path
    ssh "$host" "cat > '$2.new' && chmod 755 '$2.new' && mv -f '$2.new' '$2'" < "$1"
    echo "  $2"
}
echo "deploying to $host:$dir"
put dist/minitape "$dir/minitape"
