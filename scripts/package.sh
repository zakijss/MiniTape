#!/bin/sh
# Packs the app into dist/: minitape-V.zip, laid out to unzip onto the
# SD card root; app.json, its entry in MiniMart's catalogue; and
# SHA256SUMS. Settings files aren't included, so unzipping an update keeps
# yours.
#   VERSION=1.0.0 ./scripts/build.sh && ./scripts/package.sh 1.0.0
set -e
cd "$(dirname "$0")/.."
V=${1:?usage: package.sh VERSION}
V=${V#v}
ZIP=minitape-$V.zip
for b in minitape; do
    LC_ALL=C grep -aq "minideck-version=$V" "dist/$b" ||
        { echo "dist/$b isn't version $V: run VERSION=$V scripts/build.sh" >&2; exit 1; }
done

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
A="$STAGE/App/MiniTape"
mkdir -p "$A/licenses"
cp app/* "$A/"
cp dist/minitape "$A/"
echo "$V" > "$A/version.txt"
cp LICENSE "$A/licenses/minitape-LICENSE"
cp licenses/* "$A/licenses/"
rm -f "dist/$ZIP"
(cd "$STAGE" && COPYFILE_DISABLE=1 zip -qrX "$OLDPWD/dist/$ZIP" .)

sum=$(shasum -a 256 "dist/$ZIP" | cut -d' ' -f1)
size=$(wc -c < "dist/$ZIP" | tr -d ' ')
sed -e "s/__VERSION__/$V/g" -e "s/__SHA256__/$sum/" -e "s/__SIZE__/$size/" \
    -e "s/__RELEASED__/$(date -u +%Y-%m-%d)/" store.json > dist/app.json
(cd dist && shasum -a 256 "$ZIP" app.json > SHA256SUMS && cat SHA256SUMS)
