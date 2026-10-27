#!/bin/sh
# Проверка целостности vendor/: хэши файлов против vendor.sha256 и go mod verify.
set -eu
cd "$(dirname "$0")/.."
(cd vendor && find . -type f | LC_ALL=C sort | xargs sha256sum) | diff - vendor.sha256 > /dev/null \
  && echo "vendor/: файлы совпадают с vendor.sha256" \
  || { echo "!! vendor/ отличается от vendor.sha256"; exit 1; }
go mod verify
