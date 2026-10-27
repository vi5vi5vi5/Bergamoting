#!/bin/sh
# Сборка omd под колонку (статический arm64) и заливка в /data/opt/bin.
# На колонке нет scp и sftp-server, поэтому файл идёт через stdin ssh.
#
#   scripts/deploy.sh [хост] [ssh-порт]
set -eu

HOST=${1:-192.168.1.12}
PORT=${2:-2222}
cd "$(dirname "$0")/.."

mkdir -p bin
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/omd-arm64 ./cmd/omd
ls -l bin/omd-arm64 | awk '{print "собрано: " $5 " байт"}'

# сначала .new, потом mv: запущенный экземпляр не мешает перезаписи, а обрыв связи не оставит битый файл
ssh -p "$PORT" "root@$HOST" 'mkdir -p /data/opt/bin && cat > /data/opt/bin/omd.new && chmod +x /data/opt/bin/omd.new && mv /data/opt/bin/omd.new /data/opt/bin/omd' < bin/omd-arm64
ssh -p "$PORT" "root@$HOST" 'ls -l /data/opt/bin/omd && sha256sum /data/opt/bin/omd' < /dev/null
sha256sum bin/omd-arm64
