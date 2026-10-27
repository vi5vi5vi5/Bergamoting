#!/bin/sh
# usage: capture.sh <name> <seconds>
# Records loopback IPC (pcap) and daemon logs from the speaker into cap/<name>.*
S="/c/Users/igora/AppData/Local/Temp/claude/C--Users-igora-OneDrive-Desktop-Bergamoting-quasar-messaging/68b0fbee-2d76-41ae-9aa9-8cf6c4ff1cfa/scratchpad"
NAME=$1
DUR=${2:-300}
HOST="root@192.168.1.12"
mkdir -p "$S/cap"
ssh -p 2222 $HOST "/opt/bin/tcpdump -i lo -s 0 -U -w - 'tcp and not port 8765' 2>/dev/null & P=\$!; sleep $DUR; kill \$P" < /dev/null > "$S/cap/$NAME.pcap" &
A=$!
ssh -p 2222 $HOST "cd /data/quasar/daemons_logs; tail -n 0 -F maind.log mediad.log glagold.log 2>/dev/null & P=\$!; sleep $DUR; kill \$P" < /dev/null > "$S/cap/$NAME.log" &
B=$!
date +%T > "$S/cap/$NAME.start"
wait $A $B
date +%T > "$S/cap/$NAME.end"
ls -la "$S/cap/$NAME".*
