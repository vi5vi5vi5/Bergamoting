#!/bin/sh
# Выключает шим: снимает правило, останавливает шим и ждёт, пока platformd переподключится к mediad напрямую.
# Выполняется на колонке: ssh ... 'sh -s' < scripts/shim-off.sh
RULE="-p tcp -d 127.0.0.1 --dport 9906 ! -s 127.0.0.2 -j REDIRECT --to-ports 19906"
# прямые клиентские сокеты к 9906 (0x26B2): удалённый порт 9906, адрес источника не 127.0.0.2
direct() { awk 'NR>1 && $3 ~ /:26B2$/ && $4=="01" && $2 !~ /^0200007F:/' /proc/net/tcp | wc -l; }
while iptables -t nat -D OUTPUT $RULE 2>/dev/null; do :; done
echo "$(date -u +%T) правило снято: $(iptables -t nat -S OUTPUT | grep -c 19906) правил с 19906 осталось"
[ -f /tmp/shim-guard.pid ] && kill "$(cat /tmp/shim-guard.pid)" 2>/dev/null
rm -f /tmp/shim-guard.pid
[ -f /tmp/shim.pid ] && kill -TERM "$(cat /tmp/shim.pid)" 2>/dev/null
i=0
while ps -o pid,args | grep "omd shim" | grep -v grep > /dev/null; do
	i=$((i+1)); [ $i -gt 15 ] && { echo "шим не остановился за 15 с"; exit 2; }
	sleep 1
done
echo "$(date -u +%T) шим остановлен"
i=0
while [ "$(direct)" -lt 4 ]; do
	i=$((i+1)); [ $i -gt 90 ] && { echo "platformd не вернулся к mediad напрямую: $(direct) из 4 за 90 с"; exit 3; }
	sleep 1
done
echo "$(date -u +%T) напрямую к mediad подключено клиентских сокетов: $(direct) (спустя ~${i} с)"
rm -f /tmp/shim.deadline /tmp/shim.pid /tmp/shim.rolledback
