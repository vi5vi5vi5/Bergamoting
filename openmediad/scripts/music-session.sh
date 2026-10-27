#!/bin/sh
# Сценарий с музыкой через maind --cli + независимая запись шины (tcpdump) → captures/ИМЯ.pcap.
# Фразы и паузы заданы файлом: строки вида «секунды_ожидания<TAB>фраза» (фраза Алисе через пункт 'q').
#
#   scripts/music-session.sh ИМЯ сценарий.txt [хвост_записи_сек]
#
# Показывает Master до и после (громкость колонки не должна меняться от сценария, если в нём нет «громче/тише»).
# Звук будет слышен на колонке. Шим не нужен: запись идёт со штатного пути.
set -eu

NAME=${1:?имя записи}
PLAN=${2:?файл сценария}
TAIL=${3:-10}
HOST=${HOST:-192.168.1.12}
PORT=${PORT:-2222}
SSH="ssh -p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@$HOST"
cd "$(dirname "$0")/.."
mkdir -p captures

total=8
cmds="sleep 3"
while IFS="$(printf '\t')" read -r wait text; do
	[ -n "$wait" ] || continue
	esc=$(printf '%s' "$text" | sed "s/'/'\\''/g")
	cmds="$cmds; echo q; sleep 1; echo '$esc'; sleep $wait"
	total=$((total + wait + 2))
done < "$PLAN"
cmds="$cmds; echo x"
SECS=$((total + TAIL))

master() { $SSH "amixer sget Master 2>/dev/null | grep -o '\[[0-9]*%\]' | head -1" < /dev/null 2>/dev/null || true; }
echo "Master до: $(master)   длительность записи: ${SECS} с"

$SSH "/opt/bin/tcpdump -i lo -s 0 -U -w - 'tcp and not port 8765' 2>/dev/null & P=\$!; sleep $SECS; kill \$P" \
	< /dev/null > "captures/$NAME.pcap" 2>/dev/null &
B=$!
sleep 2
$SSH "( $cmds ) | /system/vendor/quasar/maind --cli 2>&1 | grep -v '\[threading\]\|\[info\]\|\[rate_limiter\]' | tail -40" \
	< /dev/null > "captures/$NAME.cli.log" 2>&1 || true
wait $B
echo "Master после: $(master)"
ls -l "captures/$NAME".*
