# openmediad

Свой лёгкий медиа-движок для Яндекс Станции Мини 3 (bergamot, root по SSH).
Цель — воспроизведение из любых источников (Яндекс-поток, Spotify, SoundCloud, meetup-Opus,
Bluetooth-вывод) через одну точку, с возможностью полностью заменить штатный `mediad`.

- **План работ:** [PLAN.md](PLAN.md) — фазы, проверки, развилки, бюджет памяти, результаты замеров.
- **Архитектура (читать первой, если вы переписываете код):** [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md);
  формат сообщений шины и спектра — [docs/BUS-PROTOCOL.md](docs/BUS-PROTOCOL.md).
- **Протокол шины и разбор железа:** `../quasar messaging/PROTOCOL.md` (плеер — разделы 5/8,
  замена mediad — раздел 13). Инструменты разбора — `../quasar messaging/tools/`.
- **Доступ к колонке:** `ssh -p 2222 root@192.168.1.12` (пароль пустой). Досье — `../mini3-dossier.md`.

Статус: фаза 0 выполнена (наблюдатель шины, PLAN.md §11). Фаза 1: ALSA изучена (§13), декодер CELT выбран и вендорен
(`go-opus`, THIRD_PARTY.md), связка «Go-декодер → pipe → aplay» работает и измерена (§14); Bluetooth-вывод — §15, не проверен с
наушниками. **Фаза 2 выполнена (§16):** прозрачный шим на 9906 работает (iptables REDIRECT + `omd shim`, без правок rootfs),
1826 кадров сверено побайтно без расхождений, откат и страховка проверены; постоянной установки пока нет.
**Фаза 3 выполнена (§17, §18), голос Алисы (§19) и звонки таймеров и будильников (§20) в шиме:** `omd shim -sounds` играет звонок сам (файл → PCM → aplay по кругу до release, события байт в байт как у mediad, откат на mediad). До этого: `omd shim -voice` озвучивает ответы Алисы сам (Opus → aplay, события и спектр), откат на mediad при ошибке. До этого: `omd shim -music` играет музыку Яндекса сам (AES-CTR + MP4 + декодер GStreamer колонки),
собирает события плеера побайтно как mediad (1432 события из записей, 0 расхождений); при ошибке потока команда возвращается mediad.
Спектр для дисплея и отладочный порт 6701 работают; на слух проверена только музыка, постоянной установки нет.
Решение по TTS принято: голос Алисы берём на себя (вывод, например, в наушники), значит фаза 5 — цель.

```sh
go test ./...                      # тесты (реплей реальной записи: OMD_PCAP=файл.pcap)
sh scripts/deploy.sh               # собрать под колонку и залить в /data/opt/bin/omd
sh scripts/live.sh имя 120         # живая запись: наблюдатель на колонке + дамп трафика → captures/
go run ./cmd/omd pcap -progress captures/имя.pcap
go run ./cmd/omd pcap -voice-dir captures/voice captures/имя.pcap   # голос Алисы → .ogg
# на колонке:  /data/opt/bin/omd observe [-progress] [-stats 10s] [-mono]
# на колонке:  /data/opt/bin/omd voice-play -start-delay 1 [-prespawn] [-repeat N] файл.ogg   # декодер → pipe → aplay, замер до запуска PCM
# на колонке:  /data/opt/bin/omd pcmwatch -for 60s                                          # смены состояния PCM и trigger_time
sh scripts/vendor-check.sh         # целостность vendor/ (хэши) и go mod verify

# шим фазы 2 (на колонке; в rootfs ничего не пишется, пропадает при перезагрузке):
ssh -p 2222 root@192.168.1.12 'sh -s -- 1500 "-v -mono"' < scripts/shim-on.sh   # включить (mediad перезапустится), страж снимет правило через 1500 с
ssh -p 2222 root@192.168.1.12 'sh -s' < scripts/shim-off.sh                      # выключить
sh scripts/shim-capture.sh имя 900         # запись tcpdump обеих сторон шима → captures/имя.pcap
go run ./cmd/omd shimcheck captures/имя.pcap   # сверка кадров и задержка шима по записи
sh scripts/alice-say.sh "сколько будет два плюс два"   # текстовый вопрос Алисе через maind --cli (+ pcmwatch)

# фаза 3 (музыка в шиме; шим включать с флагом -music, откат — shim-off.sh):
ssh -p 2222 root@192.168.1.12 'sh -s -- 900 "-music -voice -sounds"' < scripts/shim-on.sh
ssh -p 2222 root@192.168.1.12 'sh -s -- 10 200' < scripts/volume-clamp.sh   # на время проверок держит Master на 10 % (чтобы не мешать домашним)
sh scripts/music-session.sh имя сценарий.txt   # сценарий «секунды<TAB>фраза»: голосовые команды + запись шины в captures/
go run ./cmd/omd pcap -dump captures/имя.pcap   # дерево полей команд и событий плеера
go run ./cmd/omd timeline captures/имя.pcap     # сжатая шкала: что видела платформа (команды, состояния, перемотки)
# отладка шима с ПК: http://192.168.1.12:6701/ (журнал с пояснениями), go tool pprof http://192.168.1.12:6701/debug/pprof/profile?seconds=20
# на колонке:  /data/opt/bin/omd music-test -url URL -key HEX [-from мс] [-for 15s]   # один поток Яндекса движком music
```

Правила: всё своё в `/data/opt`, бэкап `launch.json`, каждая фаза откатывается, колонку не
сбрасывать и не обновлять.

Bluetooth-наушники: план и риски — PLAN.md §15; `sh scripts/bt-test.sh status` (только чтение), остальные команды
скрипта требуют наушников в режиме спаривания и ещё не запускались.
