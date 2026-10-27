#!/bin/sh
# Страховка шима (работает на колонке, запускается shim-on.sh): как только шим перестал жить или вышло время
# из /tmp/shim.deadline, снимает правило iptables и останавливает шим. Клиенты переподключаются к настоящему
# сервису напрямую. Время можно продлить: echo $(( $(date +%s) + 600 )) > /tmp/shim.deadline
SHIM_PID=$1
RULE="-p tcp -d 127.0.0.1 --dport 9906 ! -s 127.0.0.2 -j REDIRECT --to-ports 19906"
echo $$ > /tmp/shim-guard.pid
while :; do
	dl=$(cat /tmp/shim.deadline 2>/dev/null || echo 0)
	if ! kill -0 "$SHIM_PID" 2>/dev/null; then why="шим не жив"; break; fi
	if [ "$(date +%s)" -ge "$dl" ]; then why="вышло время"; break; fi
	sleep 1
done
while iptables -t nat -D OUTPUT $RULE 2>/dev/null; do :; done
kill -TERM "$SHIM_PID" 2>/dev/null
echo "$why $(date -u +%T)" > /tmp/shim.rolledback
rm -f /tmp/shim-guard.pid
