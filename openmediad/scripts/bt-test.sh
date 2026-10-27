#!/bin/sh
# Проверка Bluetooth-вывода на наушники. НЕ ЗАПУСКАЛСЯ: нужны реальные наушники в режиме спаривания.
# Всё выполняется на колонке через ssh; состояние Bluetooth меняется только явными командами pair/connect.
#
#   scripts/bt-test.sh status              только чтение: питание, видимость, число спаренных и подключённых
#   scripts/bt-test.sh scan [секунды]      найти устройства (наушники должны быть в режиме спаривания)
#   scripts/bt-test.sh pair AA:BB:CC:DD:EE:FF   спарить, довериться, подключить; затем вернуть Pairable: no
#   scripts/bt-test.sh play AA:BB:CC:DD:EE:FF   проиграть реальный чанк «Включаю.» в наушники через bluealsa
#   scripts/bt-test.sh disconnect AA:BB:...     отключить
#   scripts/bt-test.sh remove AA:BB:...         удалить спаривание (вернуть как было)
set -eu
HOST=${BT_HOST:-192.168.1.12}
PORT=${BT_PORT:-2222}
cd "$(dirname "$0")/.."
sshc() { ssh -p "$PORT" "root@$HOST" "$@"; }
cmd=${1:-status}
mac=${2:-}
need_mac() { [ -n "$mac" ] || { echo "нужен MAC наушников"; exit 2; }; }

case "$cmd" in
status)
  sshc 'bluetoothctl show | grep -E "Powered|Discoverable:|Pairable"; echo "спарено: $(bluetoothctl paired-devices | wc -l), подключено: $(bluetoothctl devices Connected | wc -l)"; bluealsa-aplay -L 2>&1 | head -6' < /dev/null ;;
scan)
  s=${2:-12}
  sshc "bluetoothctl pairable on >/dev/null; echo 'ищу $s с...'; bluetoothctl --timeout $s scan on >/dev/null 2>&1; bluetoothctl devices; bluetoothctl pairable off >/dev/null" < /dev/null ;;
pair)
  need_mac
  sshc "bluetoothctl pairable on; bluetoothctl pair $mac; bluetoothctl trust $mac; bluetoothctl connect $mac; sleep 3; bluetoothctl info $mac | grep -E 'Name|Paired|Trusted|Connected|UUID' ; echo '--- PCM bluealsa:'; bluealsa-aplay -L 2>&1 | head -6; bluetoothctl pairable off" < /dev/null ;;
play)
  need_mac
  sshc 'cat > /tmp/v2.ogg' < captures/voice/v2.ogg
  sshc "/data/opt/bin/omd voice-play -label 'BT: реальный чанк в наушники' -dev 'plug:bluealsa:DEV=$mac,PROFILE=a2dp' -start-delay 1 -repeat 1 /tmp/v2.ogg; rm -f /tmp/v2.ogg" < /dev/null ;;
disconnect) need_mac; sshc "bluetoothctl disconnect $mac" < /dev/null ;;
remove)     need_mac; sshc "bluetoothctl remove $mac" < /dev/null ;;
*) sed -n '2,12p' "$0"; exit 2 ;;
esac
