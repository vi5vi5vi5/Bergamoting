#!/bin/sh
# Держит громкость колонки (Master) на заданном уровне, пока идут проверки: чтобы тесты не мешали домашним.
# Платформа сама поднимает громкость на время звонка таймера и будильника (нарастание громкости будильника — alarmd), поэтому разовой установки мало.
# Выполняется на колонке:
#   ssh -p 2222 root@192.168.1.12 'sh -s -- 10 200' < scripts/volume-clamp.sh      # 10 %, 200 проверок по 0,3 с (≈1 мин), в переднем плане
#   ssh -p 2222 root@192.168.1.12 'cat > /tmp/volume-clamp.sh && (nohup sh /tmp/volume-clamp.sh 10 800 >/dev/null 2>&1 </dev/null &)' < scripts/volume-clamp.sh   # в фоне
# Команды Алисы «громкость N» не работают (smart_volume отключает sound_set_level), поэтому Master меняется напрямую через amixer.
# После проверок верните прежний уровень: `amixer set Master N%` (узнать до проверки: amixer sget Master).
P=${1:-20}
N=${2:-240}
i=0
while [ $i -lt $N ]; do
	amixer set Master ${P}% >/dev/null 2>&1
	i=$((i+1))
	sleep 0.3 2>/dev/null || sleep 1
done
