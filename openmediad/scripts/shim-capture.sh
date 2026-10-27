#!/bin/sh
# Запись трафика audioclient (до и после шима) в captures/ИМЯ.pcap: tcpdump на колонке, поток идёт на ПК.
#   scripts/shim-capture.sh имя [секунд, по умолчанию 900]
# Порт 9906 — настоящий сервис (прямые клиенты и шим с 127.0.0.2), 19906 — клиентская сторона шима.
NAME=${1:?имя записи}
DUR=${2:-900}
HOST=${HOST:-192.168.1.12}
PORT=${PORT:-2222}
cd "$(dirname "$0")/.." && mkdir -p captures
ssh -p "$PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "root@$HOST" \
	"/opt/bin/tcpdump -i lo -s 0 -U -w - 'tcp and (port 9906 or port 19906)' 2>/dev/null & P=\$!; sleep $DUR; kill \$P" \
	< /dev/null > "captures/$NAME.pcap" 2>/dev/null
ls -l "captures/$NAME.pcap"
