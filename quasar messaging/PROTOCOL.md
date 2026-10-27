# Протокол общения микросервисов Quasar на Станции Мини 3 (bergamot)

> Разбор снят с живого устройства 27.10.2026, прошивка `0.369.1.50.4794652521.20261021.117`.
> Цель — описать, как компоненты говорят друг с другом, чтобы бесшовно заменить штатный
> механизм воспроизведения музыки на нативный Spotify.
>
> Методика: снятие трафика шины `tcpdump -i lo` + логи демонов + статический разбор
> `maind` (имена proto-типов) + сверка с проектами `spotiya`, `quasard`, `yam`, `YandexStation`.
> Все номера полей protobuf восстановлены из реального трафика; где поле не подтверждено
> наблюдением, стоит пометка *(вывод)*.

---

## 0. Краткий вывод для нетерпеливых

- Вся музыка на колонке играет через **один механизм**: облако присылает директиву
  **`audio_play`** с прямой HTTP-ссылкой на поток (`strm-*.strm.yandex.net/...`), а
  проигрывает её процесс **`maind --mediad`** (сервис `audioclient`, порт `9906`) через
  GStreamer → ALSA. Плееру всё равно, что за ссылка, лишь бы это был HTTP-поток.
- Точка, где можно **перехватить голосовую команду** «включи …» / «пауза» / «дальше» —
  сервис **`aliced`** на `127.0.0.1:9875`: он транслирует финальный распознанный текст
  и состояние Алисы сообщением `AliceState`.
- Точка, где можно **отменить** штатный ответ Алисы и **управлять плеером** — тот же
  `aliced` через сообщения `Remoting` (отмена диалога, пауза/плей/следующий).
- Поэтому **для Spotify есть две рабочие стратегии** (раздел 9): «сосед» (librespot со
  своим ALSA-выводом, как `spotiya`) или «подмена потока» (кормить штатный плеер своей
  ссылкой). Spotify не отдаёт голых HTTP-mp3, поэтому librespot нужен в любом случае;
  различие — кто владеет звуковым выходом.

---

## 1. Процессы и роли

`S99quasar` → `onboot.sh` → `run.sh` → `quasar_launcher2 launch.json`. Запускается **один
бинарник `maind`** в четырёх ролях (подтверждено `ps` и `launch.json`):

| PID-роль | Что внутри | Порты, которые слушает |
|---|---|---|
| `maind --platformd` | **почти всё**: Алиса (`aliced`), спотеры, голосовой тракт, светодиоды, кнопки, громкость, сеть, glagol, iohub, директивный конвейер | 9875, 9877, 9878, 9882, 9884, 9898, 9899, 9903, 9908, 9910, 9911, 9912, 9913, 9917, 9921, 9922, 9924, 9926, 9850, 11124, 5081, 7447, 1961, 5353 |
| `maind --mediad` | **плеер** музыки и TTS-ответов (`audioclient`): GStreamer, вывод в ALSA | 9900, 9906 |
| `maind --telemetry` | телеметрия, мониторинг | 9887, 9895 |
| `maind --fluent-bitd` | сбор логов | 9832 |

Симлинки на `maind`: `led_util`, `clock_led_util`, `tcp_alsa_writer`, `quasar_client`,
`keymaster_proxy_app`, `frame_buffer_ledpattern_analyser`, `metrics_collector`.

Внешние (не-maind) демоны рядом: `dbus-daemon`, `wpa_supplicant`, `bluealsa(+aplay)`,
`bluetoothd`, `adbd` (5555), `unbound` (DNS 53), `dropbear` (SSH 2222), `chronyd`.

Диагностический вход: **`maind --cli`** (интерактивное меню, раздел 4.4) и хелпер
`/system/vendor/quasar/q` (`q full-info`, `q glagol-connections`, `q cli`, …).

---

## 2. Транспорт шины (framing + конверт)

Все сервисы Quasar общаются по **локальному TCP на `127.0.0.1`**, даже когда живут в
одном процессе (`platformd` подключается сам к себе). Один кадр:

```
┌──────────────┬───────────────────────────────┐
│ len: u32 LE  │ QuasarMessage (protobuf, len Б)│
└──────────────┴───────────────────────────────┘
```

> `len` читается как little-endian uint32 (в коде встречается и знаковый int32 — значения
> до 10 МиБ, лимит `IPC_MAX_FRAME_SIZE`). Отрицательная длина = ошибка.

### 2.1. Конверт QuasarMessage (ключевой момент)

`quasar.proto.QuasarMessage` — это большой message-«оболочка», где **каждый номер поля
соответствует отдельному типу вложенного сообщения** (как гигантский `oneof`). Чтобы не
пересериализовывать всю оболочку, на провод кладут **только два поля**:

```
QuasarMessage {
  field 252 (bytes)  = сериализованное вложенное сообщение (payload)
  field 253 (varint) = номер поля QuasarMessage, т.е. ТИП payload
}
```

Это и есть «конверт», который в `spotiya/src/voice.rs` и `yam` описан как
`{252: payload, 253: type}`. Приёмник читает `253`, узнаёт тип, и парсит `252` как
соответствующий message. **Подтверждено на всех сервисах.**

### 2.2. Таблица типов (значение поля 253 → смысл payload)

Снято с реального трафика; имена — из `strings maind` (`quasar.proto.*`).

| `253` | Payload-тип (имя в maind) | Где виден | Направление |
|---|---|---|---|
| **3** | `AliceState` | aliced 9875 | S→C (broadcast состояния) |
| **8** | команда плееру *(вывод: `MediaRequest`/AudioClient cmd)* | audioclient 9906 | C→S |
| **85** | `IOHubMessage` | iohub 9903/9910 | обе |
| **88** | iohub ack/ответ *(вывод)* | iohub | S→C |
| **127** | `AudioClientEvent` | audioclient 9906 | S→C (события плеера) |
| **183** | iohub служебное *(вывод)* | iohub_services | — |
| **207** | handshake возможностей (`SupportedFeatures`/`Endpoint`) | aliced | S→C при подключении |
| **210** | `Remoting` (вызовы методов капабилити) | aliced 9875 | обе |
| **24** | `DatabaseMetricaEvent` | metricad 9887 | C→S |
| **240/241** | network_stats запрос/ответ | network_stats 9926 | обе |
| **247** | `AudioClientEvent.AltUrlChanged` *(вывод)* | audioclient | S→C |
| **250** | update_health_check ping | 9908 | C→S |

> Прочие типы существуют (в `maind` ~352 имени `quasar.proto.*`), но в музыкальном
> сценарии не встречаются.

---

## 3. Карта сервисов и портов

Порты из `quasar.cfg` (ключ `<сервис>.port`), владелец подтверждён сопоставлением
сокет→PID на устройстве.

| Порт | Сервис | Роль | Порт | Сервис | Роль |
|---|---|---|---|---|---|
| **9875** | **aliced** | диалог Алисы, стейт | 9910 | iohub_clients | шина умного дома (клиент) |
| 9877 | wifid | Wi-Fi | 9911 | ntpd | время |
| 9878 | authd | токены/аккаунт | 9912 | setupd | первичная настройка |
| 9882 | config_capability | конфиг | 9913 | ble_initd | BLE |
| 9884 | firstrund | онбординг | 9917 | networkd | сеть |
| 9887 | metricad | метрики | 9921 | stereo_pair | стереопара |
| 9895 | monitord | мониторинг | 9922 | volume_manager | громкость |
| 9898 | brickd | подписочная блокировка | 9924 | backend | облачный бэкенд |
| **9899** | **glagold** | локальный API (наружу **1961**) | 9926 | network_stats | статистика сети |
| **9900** | **mediad** | плеер (контроллер) | 9908 | update_health_check | — |
| 9903 | iohub_services | шина умного дома (сервер) | 11124 | multiroomd | мультирум |
| **9906** | **audioclient** | плеер (аудио-клиент) | 9850 | yio_audio | аудио-init |

Наружу (0.0.0.0): `1961` glagol, `2222` SSH, `5555` ADB, `5081`, `7447` zenoh.

---

## 4. Сервис `aliced` (127.0.0.1:9875) — сердце диалога

Через него проходит всё: активация, распознанный текст, ответ облака с директивами,
управление плеером. Это **главная точка перехвата** для замены.

### 4.1. При подключении

Клиент, подключившись, получает тип **207** (handshake): `app_id=aliced`, `deviceType=bergamot`,
версия и список `supportedFeatures` (`audio_client`, `directive_sequencer`, `muzpult`,
`thin_fm_radio_client`, `scled_display`, …). Клиент со своей стороны шлёт `Remoting` на
`EndpointStorage_` / `DeviceStateCapability`, чтобы объявить себя и подписаться.

### 4.2. `AliceState` (тип 253 = 3) — то, что сервис вещает всем подписчикам

Самое важное сообщение. Структура полей (подтверждена трафиком):

| Поле | Смысл | Значения |
|---|---|---|
| **1** | **состояние Алисы** (`aliceState`) | 0=IDLE, 1=LISTENING, 2=…, 3=BUSY/THINKING, 5=SPEAKING, 6=NONE |
| **2** | **распознанная фраза** (текст) | «Включи музыку», «Пауза», «Дальше»… |
| **3** | **ResponseState** — ответ облака (см. 4.3) | присутствует на финале |
| 5 | флаг (всегда 1) | |
| 6 | версия спотера-модели | `ru-RU-bergamot-alisa-...` |
| 7 | **requestId** (UUID 36 символов) | связывает всю цепочку |
| 10 | источник запроса | `{1:1, 2:3, 3:2, 4:<uuid>, 5:<serial>, 6:"activation"}` |
| 11 | флаг | |
| 15 | **ASR-подсостояние** | `{1:текст, 2:isFinal(0/1), 3:"", 4:1}` |

**Как читать для перехвата** (ровно это делает `spotiya`): финальной фраза считается, когда
поле `1` = LISTENING(1) или BUSY(3) **и** в поле `15` одновременно `2==1` и `4==1`, при этом
`15.1` совпадает с полем `2`. Тогда `2` = окончательный текст, `7` = requestId. Фразу можно
обработать на ~1 секунду раньше, чем Алиса начнёт отвечать (пока ещё LISTENING) —
`spotiya` этим пользуется, чтобы отменить штатный ответ до его озвучки.

Активность для «подутишения» (ducking): состояния 1/2/3/5 = Алиса занята, 0/6 = свободна.

### 4.3. ResponseState (поле 3 внутри AliceState) — ответ облака с директивами

Когда приходит ответ, поле `3` содержит:

| Подполе | Смысл |
|---|---|
| 1 | текст-ответ (`outputSpeech`, напр. «Включаю.») |
| 5 | **полный JSON ответа** (`$json`) — карточки + `directives` |

В JSON ключевое — массив `response.directives`. Для музыки это:

```json
{ "name": "tts_play_placeholder", "type": "client_action", ... },
{ "name": "audio_play", "sub_name": "music", "type": "client_action",
  "payload": { "stream": { "url": "https://strm-...mp4", "codec":"AacMp4",
                           "bitrate":192, "decode_key":"...", "duration_ms":252010 },
               "metadata": { "title":"Signs", "subtitle":"Ikon", "art_image_url":"...",
                             "glagol_metadata": { "music_metadata": {...} } },
               "callbacks": { "on_started":..., "on_finished":..., "on_failed":... },
               "background_mode":"Ducking", "screen_type":"Music" } },
{ "name": "@@mm_stack_engine_get_next", "type": "server_action", ... }
```

- **`audio_play`** — собственно команда играть поток. `stream.url` — прямая HTTPS-ссылка на
  трек Яндекс Музыки. Плеер определяет кодек по содержимому; `format`/`codec` — лишь валидный
  enum. Старый плеер **не любит редиректы** (308) — облако отдаёт уже конечную ссылку.
- **`@@mm_stack_engine_get_next`** — «серверный экшн»: станция хранит его и присылает обратно,
  когда трек кончился или сказали «дальше» → облако отвечает следующим `audio_play`. Так
  устроена очередь/«Моя волна».
- **`callbacks`** (on_started/finished/failed) — только статистика (`ignore_answer:true`),
  уходят обратно в облако как `Vins.TextInput`.

### 4.4. `Remoting` (тип 253 = 210) — вызовы методов капабилити

Так управляют сервисами. Структура: поле `7` = имя капабилити (строка), далее поле с
номером метода = тело метода. Снятые в трафике и через `maind --cli` вызовы:

| Капабилити (поле 7) | Поле метода | Тело | Действие |
|---|---|---|---|
| `AliceCapability` | 8 | `{1:3}` | **отменить диалог** (CANCEL_DIALOG) |
| `AliceCapability` | 8 | `{1:0, 2:{…text_input…}}` | **текстовый запрос** (как `q`-команда) |
| `PlaybackControlCapability` | 13 | `{1:1, 5:{1:14,2:3,3:1,4:<uuid>,6:"SpotifyVoice"}, 7:0}` | **пауза** (kind=1) |
| `DirectiveSequencer` | (методы 1..300 разобраны, но все требуют корректного тела) | | добавить/обработать директиву |
| `EndpointStorage_` | 1 | `{1:<uuid>,2:2,4:"cli"}` | регистрация клиента |
| `DeviceStateCapability` | 16 | `{36:""}` | подписка на стейт |
| `AudioFilePlayerCapability` | 12 | `{1:2}` | плеер файлов |
| `TimingsManager` | 34 | события таймингов | телеметрия |

**CLI подтверждает набор команд** (`maind --cli`, меню):

```
'c' Cancel Conversation      -> Remoting AliceCapability{8:{1:3}}
'o' Player Pause             -> Remoting PlaybackControlCapability{13:...}
'p' Toggle PlayPause         'n' Next   'l' Prev   's' Start   't' Toggle
'q' Text Input Query         -> Remoting AliceCapability{8:{1:0, 2:{text_input}}}
'm' Toggle mute
```

Направление `audio_play` внутри `aliced` обрабатывает `directive_sequencer`: он собирает
цепочку директив (`alice_response → tts_play_placeholder → audio_play → @@mm_stack_engine_get_next`),
прогоняет через препроцессоры (SmartVolume, Multiroom) и маршрутизирует каждую в свою
капабилити (видно в `maind.log`: `routeDirective ... via AudioPlayerCapability`).

### 4.5. События `AliceCapability` (поле 18 внутри Remoting) — расширенный стрим

Помимо `AliceState`(3), aliced шлёт события через Remoting `AliceCapability` поле 18, где
подполе `1` = тип события: 0=стейт, 2=начало запроса, 3=принят ответ(local_action),
4=alice_response, 5=инфо аккаунта, 6=таймаут, 7=запрос начат. Поле `18.2`/`18.4` несут тот
же JSON ответа, что и `AliceState.3`.

---

## 5. Сервисы плеера `mediad` / `audioclient` (9900 / 9906)

`maind --mediad` — отдельный процесс. Снаружи к нему идут два потока на `audioclient`(9906):

### 5.1. Команда плееру (тип 253 = 8, направление platformd → mediad)

Основное поле `1` = описание трека/потока для старта, `22` = идентификатор плеера.
Разобранные поля `1.*` (подтверждены):

| Поле | Смысл | Поле | Смысл |
|---|---|---|---|
| 1.1 | **id трека** (`44683635`) | 1.9 | metadata (title/artist/cover/glagol_metadata) |
| 1.2 | **url потока** (https) | 1.21 | нормализация громкости (`true_peak`,`loudness`) |
| 1.3 | offset_ms (для resume) | 1.35 | кроссфейд/фейды |
| 1.4 | флаг (1) | 1.39 | **decode_key** (AES-ключ трека) |
| 1.7 | **весь payload audio_play как JSON** | 1.42 | alt_urls (резервные зеркала) |
| 1.8 | ids (requestId, sub_name, parentId) | 1.45 | bitrate (192) |
| | | 1.46 | duration_ms |

Поле `22` = `{1:тип, 2:playerId-uuid, 3:streamId, 4:0}`. Другие верхнеуровневые команды:
`29` = кадр Ogg/Opus (для TTS-потоков), `30/31` = управление потоком, `35` = конфиг
эквалайзера, `4` (пустое) = стоп/пауза ветка, `39` = фокус/активность, `50` = обновление
кроссфейда, `52` = строковая причина (отладка: `prefetchDirective`, `AUDIO_PLAY directive`,
`completeDirective: PAUSE branch`).

### 5.2. События плеера `AudioClientEvent` (тип 253 = 127, mediad → platformd)

Поток статуса (летит часто во время игры). Поля:

| Поле | Смысл |
|---|---|
| 1 | **event** (тип события: 1=prepare, 2=progress, 5=служебное, 6=state) |
| 2 | **state** (1=init, 3=playing, 4=paused, 5=…) |
| 3 | описание трека (копия команды) |
| 4 | плеер `{1:тип, 2:playerId, 3:streamId}` |
| 3.28 | **позиция, мс** |
| 3.12 | индекс позиции (секунды) |
| 14 | массив `{1:mark/vsid/buffering/rss_mem, 2:…, 3:…}` — диагностика |

Пауза наблюдается как команда `{4:"", 52:"completeDirective: PAUSE branch"}` → событие
`st=4`. Resume — новая команда `audio_play` с `offset_ms` = сохранённая позиция.

### 5.3. GStreamer-конвейер (из `quasar.cfg`, `audioclient.playbackParams`)

Трек (`TRACK`/`MEDIA_PIPELINE`):
```
souphttpsrc location={uri} ! {opt_aes_decoding} ! decodebin ! audioconvert !
audioresample ! scaletempo ! caps(S16LE,48k,stereo) ! {normalization} !
volume name=volume0 ! volume name=yio_fade ! equalizer ! spectrum ! alsasink device=default
```
TTS-ответ Алисы (`OPUS_STREAM`): `appsrc ! opusparse ! opusdec ! volume ! spectrum ! alsasink`.
Всё идёт в ALSA `default` → `speakers` (plug) → `dmix_speakers` (общий смеситель, ключ IPC 1024,
48 кГц, 2 канала, буфер 160 мс) → `hw:0,0`; громкость — аппаратный контрол `Master` (проверено на
Мини 3 27.10.2026; прежняя запись про `softvol` была взята из вики для Лайт и неверна).
`spectrum` 20 Гц — источник визуализации на дисплее-часах.

**Вывод для замены:** любой HTTP-поток, который понимает `decodebin`, играется этим же
конвейером. `maind --gst-launch "..."` позволяет запустить произвольный пайплайн вручную.

---

## 6. Шина умного дома `iohub` (9903 / 9910) — тип 85 (`IOHubMessage`)

Самый «болтливый» канал. Несёт две вещи:
- **`SDKState`** (поле `34`) — полное состояние устройства: то же `AliceState.2` (стейт),
  плюс `8.6.3` = текущий трек с `scenario_meta` (base64 `content_id`, `playback_context`,
  `queue_item`), `9` = метаданные (title/artist/cover), `19` = инфо аккаунта.
  Это то, что видят интеграции (и что отдаётся наружу через glagol как `playerState`).
- **`Spectrum`** (поле `46`) — спектр звука 20 раз/с (`IOEvent.Spectrum`): `4` = байты полос,
  `7` = playerInstanceId. Используется подсветкой/дисплеем.

`iohub_clients`(9910) ↔ `iohub_services`(9903) — зеркальные концы одной шины.

---

## 7. Облачный тракт uniproxy (для контекста)

Распознавание речи — **в облаке** (`withLocalVins:false`). На устройстве только спотеры
(«алиса», ~25 команд, VAD, биометрия). Тракт:

```
4 PDM-мик → Yandex VQE (AEC/beamforming/NS) → канал vqe_0 →
  спотер "алиса" → earcon+LED → Ogg/Opus →
  wss://uniproxy.alice.yandex.net/uni.ws  [protobuf "AAPI", zstd]
    → ASR (quasar-general) → VINS/Megamind → AliceResponse(директивы) + TtsSpeak
```

Протокол AAPI: бинарный WS-кадр `"AAPI"` + `NAlice.NAliceApi.TClientMessage`/`TServerMessage`.
Схемы восстановлены в `quasard` (`alice_server/aliceapi.desc`, 176 файлов) и `v3rt3p/quasar-protos`.
Ключевые сообщения (номера полей из `.desc`):

- **TClientMessage.TEvent**: 1 Header, 2 TextInput, 3 VoiceInput, 7 MatchedUser, 11 CancelRequest,
  13 TtsGenerate, 5 SynchronizeStateRequest, 28 RequestStat, 33 Ping.
- **TServerMessage.TEvent**: 2 AliceResponse, 5 Push, 13 AsrResult, 14 TtsSpeak, 16 SpotterValidation,
  10 InputStartAck, 24 SynchronizeStateResponse.
- **TDirective**: 1 Type, 2 Name, 3 AnalyticsType(sub_name), 5 Payload, 15 PayloadRaw, 8 IsLedSilent,
  20 DirectiveType, 21 Channel. (Это те самые `audio_play` и пр.)

Пиннинга сертификатов нет — `uniProxyUrl` можно увести на `ws://` свой сервер (основа `quasard`).

---

## 8. Полный сценарий «Алиса, включи музыку» (по шагам, с таймингом из записи)

```
 t+0.0  спотер "алиса" (alice_capability onPhraseSpotted)
        → aliced: AliceState{1:LISTENING}, добавлена директива listen.VOICE_INPUT
        → audio focus = DIALOG_CHANNEL
 t+0.0  Ogg/Opus микрофона → uniproxy (AAPI VoiceInput)
 t+1.4  AliceState{1:LISTENING, 2:"Включи музыку", 15:{2:1,4:1}}  ← ФИНАЛЬНЫЙ текст
 t+1.4  AliceState{1:BUSY}  (THINKING)
 t+2.0  облако: TTS.Speak (поток 496) + AliceResponse
        → aliced: AliceState{3:{1:"Включаю.", 5:{JSON с директивами}}}
        → music_scenario: "relevant"
 t+2.0  directive_sequencer: цепочка
        [alice_response, tts_play_placeholder, audio_play, @@mm_stack_engine_get_next]
 t+2.0  AliceState{1:SPEAKING, outputSpeech:"Включаю."}  (играет TTS через OPUS_STREAM)
 t+3.0  routeDirective audio_play via AudioPlayerCapability
        → mediad(9906) команда(тип 8){1:{url, decode_key, metadata, ...}, 22:{playerId}}
        → GStreamer: souphttpsrc(strm-...mp4) ! decodebin ! ... ! alsasink
        → audio focus = CONTENT_CHANNEL
 t+3.1  mediad: AudioClientEvent(тип 127){1:2, 3.28:pos} — пошёл прогресс
 ...    каждую секунду события прогресса; iohub вещает SDKState + Spectrum
 t+32   prefetch следующего трека (@@mm_stack_engine_get_next → новый audio_play)
```

Пауза/дальше/стоп (голос или кнопка) идут тем же путём: кнопка →
`PlaybackControlCapability` → `audio_stop`/`audio_player_continue` для mediad; голос →
облако возвращает директивы `clear_queue` / новый `audio_play`.

---

## 9. glagol (порт 1961) — внешний JSON-API

«Официальная» точка интеграции (её использует Home Assistant `AlexxIT/YandexStation`).
WebSocket поверх TLS: `wss://<ip>:1961`, самоподписанный серт (проверку отключают,
`ssl=False`). **Нужен device token**: `GET https://quasar.yandex.net/glagol/token`
с OAuth-аккаунтом Яндекса (т.е. это не чисто локально). Без токена сервер рвёт соединение
`4000 Invalid token` (проверено).

Формат запроса:
```json
{ "conversationToken": "<device token>", "id": "<uuid>",
  "payload": { "command": "<...>" }, "sentTime": <ms> }
```

Команды (из `YandexStation`): `softwareVersion`, `ping`, `sendText` (произнести/выполнить
текст), `playMusic` (`{type, id}` — альбом/трек/плейлист Яндекс Музыки), `play`, `stop`,
`next`, `prev`, `rewind`, `setVolume`, `shuffle`, `repeat`, `serverAction`,
**`externalCommandBypass`** (`{data: base64(protobuf{1:name, 2:payload})}` — произвольная
директива, в т.ч. `audio_play` с любым URL — так HA играет внешние ссылки). Ответ несёт
`state.playerState` (трек, позиция) и `vinsResponse`.

> Для локальной замены glagol менее удобен, чем прямой `aliced:9875`: требует облачный токен
> и TLS. Но `externalCommandBypass` → `audio_play` — самый простой способ заставить штатный
> плеер играть свою ссылку, не трогая aliced.

---

## 10. Точки для бесшовной замены на нативный Spotify

Три стратегии, от простой к глубокой.

### Стратегия A — «сосед» (librespot со своим ALSA), как `spotiya`  ⭐ рекомендуемая

Отдельный процесс (Spotify Connect receiver на `librespot`) играет **сам в ALSA**, в обход
mediad. Интеграция с Алисой — только чтение/управление через `aliced:9875`:

1. **Подписаться** на `aliced:9875`, читать `AliceState`(тип 3). На финальной фразе
   (раздел 4.2) разобрать интент: «включи <X>» → поиск в Spotify; «пауза/дальше/назад/
   продолжи/громче/тише» → команда librespot.
2. **Отменить штатный ответ Алисы**, чтобы она не включила Яндекс Музыку: послать
   `Remoting AliceCapability{8:{1:3}}` (CANCEL_DIALOG) + `PlaybackControlCapability` pause.
   Делать это, пока состояние ещё LISTENING (поймать финал на ~1 с раньше) — тогда отмена
   успевает уйти до озвучки отказа.
3. **Ducking**: пока `AliceState.1 ∈ {1,2,3,5}` — приглушать свой PCM (как `spotiya`: до 20%
   за 80 мс, вернуть за 300 мс).
4. **Арбитраж ALSA/кнопок** как в `spotiya` профиле `mini2`→сделать профиль для `bergamot`:
   свой ALSA (`default`/`Master`), чтение кнопок (`/dev/input/event*`), кольцо `/dev/fb0`,
   часы `/sys/class/leds/bergamot:white:clock*`.

Плюсы: не трогает mediad и облако, Алиса для остального работает штатно, нативное качество
Spotify. Минусы: нужен профиль под Мини 3 (кнопки/дисплей/ALSA), нужен Spotify Premium,
librespot под aarch64. **Это ровно путь `klockky/spotiya`** — осталось добавить профиль
`bergamot` (сейчас там `mini1`/`mini2`/`generic`; `generic` заведётся сразу, но без кнопок/
кольца/дисплея и с `voice:{protocol:"quasar", address:"127.0.0.1:9875"}`).

### Стратегия B — «подмена потока» (кормить штатный плеер своей ссылкой)

Через `audio_play` (локально `Remoting`→DirectiveSequencer, или через glagol
`externalCommandBypass`) отдать `stream.url` на локальный HTTP-прокси, который librespot
отдаёт как PCM/MP3. Плюс: используется штатный mediad (кнопки, дисплей, громкость, ducking
«бесплатно»). Минус: Spotify не даёт голых URL — всё равно нужен librespot+локальный HTTP-
мост, редиректы плеер не любит, DRM-ключи Spotify не вписать в `decode_key` (он для Яндекса).

### Стратегия C — полная подмена облака (`quasard`)

Увести домены Яндекса/`uniproxy` на свой сервер (DNS+серт, пиннинга нет). Сервер сам
отвечает на активацию и голосовой цикл, музыку берёт откуда угодно. Это `amirilivice/quasard`
/ `v3rt3p/quasar-server`. Максимальный контроль, но теряется голос/навыки Алисы (свой TTS),
нужен GPU для ASR/TTS. Для «только Spotify» — избыточно.

**Рекомендация:** стратегия A (форк/профиль `spotiya` под `bergamot`). Она ровно «бесшовная
замена механизма воспроизведения»: Алиса слышит команду, своё воспроизведение Яндекса
отменяется, играет Spotify, кнопки/подсветка/громкость — свои.

---

## 11. Готовые проекты (что откуда брать)

| Проект | Что делает | Полезное для нас |
|---|---|---|
| **klockky/spotiya** | Spotify Connect для Мини 1/2 (Rust+librespot) | **основа стратегии A**: разбор `AliceState` на 9875 (`src/voice.rs`), отмена диалога, ducking, профили устройства. Нужен профиль `bergamot`. |
| **amirilivice/quasard** | полная замена облака (uniproxy+ASR+LLM+TTS+музыка) | схемы AAPI (`aliceapi.desc`), структура `audio_play`, `music.py` (как собрать директиву плеера) |
| **v3rt3p/quasar-protos** | proto-файлы из `maind` Станции 2 | имена/поля Alice API |
| **v3rt3p/quasar-server**, z81 | эмулятор бэкенда Quasar (TS) | роутеры uniproxy/oauth/quasar |
| **nyakokitsu/yam** | менеджер пакетов + Lua-плагины | разбор конверта 252/253, `ipc.send/next` на 9875, `cancel_alice_dialog`, `IPC_PROTOCOL.md` |
| **AlexxIT/YandexStation** | интеграция HA через glagol | команды glagol 1961, `externalCommandBypass`, `audio_play_command` |

---

## 12. Инструменты воспроизведения (как снять заново)

Всё, что использовалось, лежит в `C:\...\scratchpad\tools\` (и копии данных в `scratchpad\cap\`):

- **Снять трафик + логи** (на устройстве стоит `tcpdump` из `yse install tcpdump`):
  ```sh
  ssh -p 2222 root@192.168.1.12 '/opt/bin/tcpdump -i lo -s0 -U -w - "tcp and not port 8765"' > cap.pcap
  ssh -p 2222 root@192.168.1.12 'tail -F /data/quasar/daemons_logs/maind.log mediad.log'
  ```
- **Декодер шины** `tools/qdecode.py cap.pcap quasar.cfg` — разбирает кадры
  `[len][252/253-конверт]` в JSON (имена сервисов берёт из `quasar.cfg`). Автоматически
  распознаёт вложенный protobuf/JSON/строки/байты.
- **Карта полей** `tools/infer.py cap.jsonl <svc>` — строит дерево путей `поле.поле` со
  счётчиками, типами и примерами значений (так восстановлены все таблицы выше).
- **Клиент IPC** `tools/qipc.py` — собирает конверт и шлёт кадр на сервис (через SSH-туннель
  `ssh -L 19875:127.0.0.1:9875`). Так проверены методы `Remoting`.
- **Статический разбор** `maind`: `strings` → `quasar.proto.*` имена типов и методов
  `Remoting.*Method`.

> Напоминание из досье: своё — в `/data/opt`, бинарники статикой под aarch64, CGO off,
> rootfs править через `mount -o remount,rw /` + `sync`. Не сбрасывать колонку, не обновлять.

---

## 13. Свой медиа-движок вместо `mediad`: шим vs полная замена

Идея: не подменять облако, а поставить **на колонку** свой универсальный медиа-движок и
отказаться от штатного `mediad` так, чтобы остальные компоненты этого не заметили.

### 13.1. Сначала факты про нагрузку (замерено 27.10.2026, 10 с простоя)

| Роль | CPU (доля 1 ядра) | RSS |
|---|---|---|
| `--mediad` | **≈0%** (1 jiffie/10с) | 33 МБ |
| `--telemetry` | ≈0% | 23 МБ |
| `--fluent-bitd` | ≈0% | 21 МБ |
| **`--platformd`** | **≈132%** (1.3 ядра постоянно) | 55 МБ |

`loadavg ≈ 1.5` — это **`platformd`**, а не `mediad`. Вхолостую `mediad` спит (GStreamer в
`GST_STATE_NULL`). Фон даёт всегда-включённое прослушивание: захват 4 мик 48 кГц → Yandex VQE
(AEC/beamforming/NS) → спотеры «алиса»+команды → VAD → биометрия. Это неотключаемо, пока нужна
активация по голосу.

**Вывод:** замена `mediad` даёт **≈0% экономии CPU** и ~33 МБ RAM (часть — общие страницы).
Единственный рычаг фоновой нагрузки — `platformd` (уши Алисы); урезать его = отказаться от
голосовой активации (отдельный проект «push-to-talk / без Алисы»). Поэтому свой движок имеет
смысл делать ради **функции** (универсальное воспроизведение, полный контроль звука), а не ради CPU.

### 13.2. Что, кроме музыки, идёт через `mediad` (критично для прозрачности)

Через `audioclient:9906` проходит **не только музыка**:
- **Голос Алисы (TTS)** — Opus-кадры приходят командой тип 8 поле `29` → `appsrc ! opusparse !
  opusdec ! alsasink` (в записи «Включаю.» проиграл PID mediad). Выкинешь mediad без замены
  этого — Алиса онемеет.
- **Системные звуки**, **будильники/таймеры/напоминания**, **Bluetooth-плеер** (`BLUETOOTH_STREAM`).
- `update_health_check` (9908) пингует `mediad` (тип 250 `{1:"mediad"}`) — нет ответа → система
  считает сервис мёртвым.
- `callbacks` трека (on_started/finished) уходят обратно как `Vins.TextInput` → без них ломается
  очередь и «дальше»/«Моя волна».

### 13.3. Путь A — шим (перехватчик) ⭐ рекомендуется

Наш процесс занимает **9906**, штатный `mediad` переносим на другой порт и держим живым;
проксируем шину 1:1 в обе стороны, но **`audio_play` c `sub_name:"music"`** заворачиваем на свой
движок, а TTS/будильники/BT/системные звуки прозрачно пропускаем в настоящий `mediad`.

```
platformd ──9906──> [ШИМ] ──музыка (audio_play)──> наш движок (librespot/URL) ─┐
                      │                                                         ├─> ALSA (арбитраж)
                      └──TTS/alarm/BT/прочее──> настоящий mediad (alt-порт) ────┘
events(127)/health <──┘  (для музыки синтезируем байт-совместимые события из разделов 5.1/5.2)
```

Плюсы: малая поверхность, Алиса/таймеры/BT целы, низкий риск. Минусы: нужен арбитраж ALSA
(как у spotiya/airplay — штатный стек держит `pcmC0D0p`, чужой вывод ловит EBUSY), и аккуратная
генерация событий `AudioClientEvent`(127), чтобы iohub `SDKState` / glagol `playerState` / дисплей
не «отвалились».

### 13.4. Путь B — полная замена роли `mediad`

Свой бинарник (статика aarch64, сырой protobuf) полностью отыгрывает роль. Чек-лист того, что
нужно реализовать **байт-совместимо**, иначе компоненты заметят:

1. Бинд **9900** и **9906**; framing `[u32 LE len][конверт 252/253]`.
2. **Handshake 207** при подключении с тем же списком `supportedFeatures` (раздел 4.1).
3. Регистрация клиентом у `aliced`/`iohub` (Remoting `EndpointStorage_`, `DeviceStateCapability`).
4. Приём команд **тип 8**: `audio_play`(start), pause(поле 4), stop, prefetch(поле 28),
   crossfade(50), equalizer(35), **TTS-поток Opus**(29/30/31), focus(39), volume.
5. Выдача событий **тип 127** (`AudioClientEvent`): состояния 1/3/4, прогресс `3.28` (позиция, ~1 Гц),
   buffering, start/stop и **эхо дескриптора трека** (чтобы `SDKState`/`playerState` были верны).
6. **Своё воспроизведение и музыки, и TTS** (нужен Opus-декодер → ALSA для голоса Алисы).
7. Ответ на `update_health_check` (9908).
8. Проброс `callbacks` (on_started/finished) обратно, иначе очередь/«дальше» ломаются.
9. Снять узел `mediad` из `/system/vendor/quasar/launch.json` (rootfs rw), свой демон — из
   `init.d/S99z…`, с собственным супервизором (штатный `quasar_launcher2` его больше не нянчит).

Плюсы: полный контроль звука, мы владеем ALSA целиком (проще микс/дакинг). Минусы: большой объём,
широкая поверхность риска (ошибка в TTS/событиях/health задевает диалог, будильники, дисплей,
мультирум), CPU-выигрыш ≈0.

### 13.5. Рекомендация

Так как вхолостую `mediad` CPU не ест, «свой универсальный движок» разумнее строить как **шим (A)**:
тот же результат (наш движок играет что угодно, система не замечает), но без риска онеметь или
потерять будильники. Полную замену (B) делать только если цель — именно тотальный контроль над
звуковым трактом. Для «просто Spotify» достаточно **стратегии A из раздела 10** (librespot-сосед),
которая ещё проще шима и ничего в `mediad` не трогает.
