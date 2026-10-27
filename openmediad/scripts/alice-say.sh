#!/bin/sh
# Текстовый запрос к Алисе через maind --cli (пункт 'q'): ответ звучит на колонке как обычно.
#   scripts/alice-say.sh "сколько будет два плюс два" [секунд ждать ответ, по умолчанию 12]
# Параллельно pcmwatch печатает смены состояния PCM, чтобы было видно, что звук пошёл.
TEXT=$1
WAIT=${2:-12}
HOST=${HOST:-192.168.1.12}
PORT=${PORT:-2222}
esc=$(printf '%s' "$TEXT" | sed "s/'/'\\''/g")
ssh -p "$PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "root@$HOST" "sh -s" <<REMOTE 2>&1 | grep -v 'Warning: Permanently\|no version information'
/data/opt/bin/omd pcmwatch -for $((WAIT + 6))s > /tmp/alice-say.pcm 2>&1 &
( sleep 3; echo q; sleep 1; echo '$esc'; sleep $WAIT; echo x ) | /system/vendor/quasar/maind --cli 2>&1 | grep -v '\[threading\]\|\[info\]\|\[rate_limiter\]' | tail -15
sleep 3
echo '--- pcmwatch'; cat /tmp/alice-say.pcm; rm -f /tmp/alice-say.pcm
REMOTE
