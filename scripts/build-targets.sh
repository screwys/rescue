#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
for platform in linux/amd64 linux/arm64 linux/386 linux/arm freebsd/amd64 freebsd/arm64 freebsd/386 freebsd/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 windows/386; do
  goos=${platform%/*}
  goarch=${platform#*/}
  echo "Building $platform"
  ext=""
  if [ "$goos" = windows ]; then ext=.exe; fi
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM=6 go build -trimpath -o "dist/rescue-$goos-$goarch$ext" .
done
