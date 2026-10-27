#!/bin/sh
# Включает шим на колонке (выполняется там: ssh ... 'sh -s -- [секунды]' < scripts/shim-on.sh).
# 1) omd shim слушает 127.0.0.1:19906 и ходит к настоящему audioclient (9906) с адреса 127.0.0.2;
# 2) правило iptables nat OUTPUT заворачивает на него новые соединения к 9906 (кроме самого шима);
# 3) mediad перезапускается штатным лаунчером, platformd переподключается уже через шим.
# В rootfs ничего не пишется: правило и процессы исчезают при перезагрузке. Откат: shim-off.sh.
# Страховка: shim-guard.sh снимает правило, если шим умер или вышло время (по умолчанию 1500 с).
# Шим запускается с nice -10: на загруженной колонке это сокращает редкие задержки в несколько миллисекунд.
LIFE=${1:-1500}
SHIM_ARGS=${2:-}   # дополнительные флаги шима, например "-v -mono"
OMD=/data/opt/bin/omd
GUARD=/data/opt/bin/shim-guard.sh
RULE="-p tcp -d 127.0.0.1 --dport 9906 ! -s 127.0.0.2 -j REDIRECT --to-ports 19906"
[ -x $OMD ] && [ -f $GUARD ] || { echo "нет $OMD или $GUARD"; exit 1; }
if iptables -t nat -S OUTPUT | grep -q -- "--to-ports 19906"; then echo "уже включено"; exit 1; fi

mediad_pid() { ps -o pid,args | grep "maind --mediad" | grep -v grep | awk '{print $1}' | head -1; }
# соединения, принятые шимом (локальный порт 19906 = 0x4DC2, ESTABLISHED): сокет клиента при REDIRECT
# по-прежнему «думает», что говорит с 9906, поэтому считаем со стороны шима
via_shim() { awk 'NR>1 && $2 ~ /:4DC2$/ && $4=="01"' /proc/net/tcp | wc -l; }
# откат при сбое: правило долой, шим и страховку остановить; клиенты вернутся напрямую
bail() {
	while iptables -t nat -D OUTPUT $RULE 2>/dev/null; do :; done
	kill -TERM "$SHIM" 2>/dev/null
	[ -f /tmp/shim-guard.pid ] && kill "$(cat /tmp/shim-guard.pid)" 2>/dev/null
	echo "$1 — правило снято, шим остановлен"
	exit "$2"
}

echo $(( $(date +%s) + LIFE )) > /tmp/shim.deadline
rm -f /tmp/shim.rolledback
nohup nice -n -10 $OMD shim -stats 30s $SHIM_ARGS >> /tmp/shim.log 2>&1 < /dev/null &
SHIM=$!
echo $SHIM > /tmp/shim.pid
sleep 1
kill -0 $SHIM 2>/dev/null || { echo "шим не запустился:"; tail -5 /tmp/shim.log; exit 1; }
nohup sh $GUARD $SHIM > /dev/null 2>&1 < /dev/null &
sleep 1
iptables -t nat -I OUTPUT 1 $RULE || { echo "не удалось добавить правило"; kill $SHIM; exit 1; }
echo "$(date -u +%T) шим $SHIM запущен, правило добавлено"

OLD=$(mediad_pid)
[ -n "$OLD" ] || bail "mediad не найден" 5
echo "$(date -u +%T) перезапуск mediad (pid $OLD)"
kill -TERM "$OLD"
i=0
while kill -0 "$OLD" 2>/dev/null; do
	i=$((i+1)); [ $i -gt 30 ] && bail "mediad не завершился за 30 с (SIGKILL не использую)" 2
	sleep 1
done
echo "$(date -u +%T) mediad завершился за ~${i} с"
i=0
while [ -z "$(mediad_pid)" ]; do
	i=$((i+1)); [ $i -gt 60 ] && bail "лаунчер не поднял mediad за 60 с (колонка без mediad: нужна перезагрузка питанием)" 3
	sleep 1
done
echo "$(date -u +%T) новый mediad pid $(mediad_pid)"
i=0
while [ "$(via_shim)" -lt 4 ]; do
	i=$((i+1)); [ $i -gt 90 ] && bail "platformd подключился к шиму не полностью: $(via_shim) из 4 за 90 с" 4
	sleep 1
done
echo "$(date -u +%T) через шим подключено клиентских сокетов: $(via_shim) (спустя ~${i} с)"
