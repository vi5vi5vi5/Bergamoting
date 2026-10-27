# celtcheck — сверка чисто-Go декодера Opus с эталоном колонки

Вложенный модуль (свой `go.mod`): основной модуль `openmediad` сторонних зависимостей не получает.
Проверялся `github.com/tphakala/go-opus` v1.3.0 (по умолчанию; `gopus` включается тегом `-tags gopus`
и в сверке не участвовал).

## Как воспроизвести
1. Достать голос Алисы из записи шины: `go run ./cmd/omd pcap -voice-dir captures/voice captures/имя.pcap`.
2. Получить эталон — тихо декодировать чанк родным GStreamer колонки в файл (звука нет):
   ```sh
   cd /system/vendor/quasar && LD_LIBRARY_PATH=/system/vendor/quasar/libs ./maind --gst-launch \
     "filesrc location=/tmp/v1.ogg ! decodebin ! audioconvert ! audioresample ! \
      audio/x-raw,format=S16LE,rate=48000,channels=1 ! filesink location=/tmp/v1.pcm"
   ```
   Положить рядом: `v1.ogg` и `v1.ref.pcm` (S16LE, моно, 48 кГц).
3. Сверка и замер: `go build -mod=mod -o celtcheck . && ./celtcheck -bench 20 v1.ogg`;
   только ядро декодера с замером памяти: `./celtcheck -core 20 v1.ogg v2.ogg`.
4. На колонку: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=mod -trimpath -ldflags="-s -w" -o celtcheck-arm64 .`
   залить в `/tmp` (это RAM), запустить с `nice -n 5`, потом удалить.

Результаты и выводы — `../../PLAN.md`, раздел 13. Прибор памяти (`mem_linux.go`) намеренно не выделяет память
между замерами: первая версия искажала результат собственными аллокациями.
