#!/bin/sh
# Серия коротких текстовых вопросов Алисе (maind --cli, пункт 'q') с записью смен состояния PCM: по ней считается
# задержка «команда голоса пришла → ядро запустило PCM». Выполняется на колонке: ssh ... 'sh -s' < scripts/lat-series.sh
# Первой строкой печатается привязка часов: время UTC и CLOCK_MONOTONIC в один момент (по ней метки tcpdump
# переводятся в монотонное время pcmwatch).
OMD=/data/opt/bin/omd
N=6
rm -f /tmp/lat.pcm /tmp/lat.cal
$OMD pcmwatch -for $((N * 17 + 12))s > /tmp/lat.pcm 2>&1 &
$OMD observe -mono -aliced "" -audio 127.0.0.1:9906 > /tmp/lat.cal 2>&1 &
CAL=$!
sleep 2
kill -TERM $CAL 2>/dev/null
i=0
for P in "сколько будет три плюс пять" "сколько будет два плюс два" "как тебя зовут" "сколько будет шесть плюс семь" "скажи привет" "сколько будет девять минус три"; do
	i=$((i+1))
	( sleep 2; echo q; sleep 1; echo "$P"; sleep 11; echo x ) | /system/vendor/quasar/maind --cli > /dev/null 2>&1
	sleep 1
done
sleep 4
echo "--- calibration"; head -1 /tmp/lat.cal
echo "--- pcmwatch"; cat /tmp/lat.pcm
rm -f /tmp/lat.pcm /tmp/lat.cal
