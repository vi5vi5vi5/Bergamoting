package main

import (
	"context"
	"fmt"
	"net"
	"path"
	"strings"

	"openmediad/internal/clock"
	"openmediad/internal/dbg"
	"openmediad/internal/music"
	"openmediad/internal/obs"
	"openmediad/internal/player"
	"openmediad/internal/quasar"
	"openmediad/internal/shim"
	"openmediad/internal/sound"
)

// debugNotes переводит решения автомата плеера в записи журнала с пояснениями.
func debugNotes(hub *dbg.Hub) func(kind, id, detail string) {
	id8 := func(id string) string { return id[:min(8, len(id))] }
	return func(kind, id, detail string) {
		lvl, msg, why := dbg.Info, "", ""
		switch kind {
		case "prefetch":
			msg = fmt.Sprintf("предзагрузка трека «%s»", detail)
			why = "Платформа просит заранее открыть следующий трек. Шим начал скачивать и декодировать его; пока события для платформы не нужны."
		case "play-warm":
			msg = fmt.Sprintf("запуск «%s»: поток уже открыт", detail)
			why = "Быстрый старт: звук уже в буфере, остаётся передать его на динамик."
		case "play-cold":
			msg = fmt.Sprintf("запуск «%s»: открываю поток", detail)
			why = "Заранее открыт не был: нужно подключиться к серверу Яндекса и запустить декодер (≈0,5 с до звука)."
		case "play-resume":
			msg = "возобновление с паузы: " + detail
			why = "Платформа прислала новую команду play для трека, который стоит на паузе. Берём готовый источник, заново ничего не скачиваем."
		case "pause":
			msg = "пауза плеера " + id8(id)
			why = "Вывод остановлен, источник и позиция сохранены (как кэш плееров у mediad)."
		case "retire":
			msg = fmt.Sprintf("плеер %s закрыт: %s", id8(id), detail)
			why = "Новый play вытеснил старый плеер; платформа получит у старого событие «закончил»."
		case "evict":
			msg = fmt.Sprintf("кэш паузы плеера %s убран", id8(id))
			why = "На паузе держим один плеер (как mediad), чтобы не копить декодеры в памяти: " + detail + "."
		case "expire":
			msg = fmt.Sprintf("плеер %s на паузе закрыт по времени", id8(id))
			why = "Пауза длилась дольше 30 минут (так же закрывает кэш mediad); «продолжи» теперь начнёт трек заново."
		case "release":
			msg = "плеер " + id8(id) + " освобождён"
			why = "Платформа отменила предзагрузку или закрыла плеер; декодер и скачивание остановлены."
		case "focus":
			lvl = dbg.Info
			if detail == "true" {
				msg, why = "приглушаю музыку", "Алиса слушает или говорит (аудиофокус у диалога). Громкость музыки снижается до 25 % плавно."
			} else {
				msg, why = "возвращаю громкость музыки", "Диалог закончился, громкость музыки плавно возвращается."
			}
		case "seek":
			msg = fmt.Sprintf("перемотка плеера %s: %s", id8(id), detail)
			why = "Команда «перемотай»: позиция сдвинута сразу, поток откроется заново с нужного места (Range-запрос)."
		case "finished":
			msg = fmt.Sprintf("плеер %s: %s", id8(id), detail)
			why = "Платформа получит событие «закончил» и сама запустит следующий трек."
		case "voice-start":
			msg = "ответ Алисы: начало (плеер " + id8(id) + ")"
			why = "Платформа начинает озвучивать ответ. Шим поднял aplay и ждёт звук; настоящему mediad ответ не передан."
		case "voice-playing":
			msg = "ответ Алисы: пошёл звук"
			why = "Первые отсчёты декодированного голоса переданы на динамик; платформа получила PLAYING."
		case "voice-ended":
			msg = "ответ Алисы: " + detail
			why = "Звук доигран и aplay закрыт; платформа получила ENDED и пришлёт release."
		case "voice-release":
			msg = "ответ Алисы: плеер " + id8(id) + " освобождён"
			why = "Платформа закрыла голосовой плеер (штатно после ENDED; раньше — если ответ перебили)."
		case "voice-failed":
			lvl = dbg.Warn
			msg = fmt.Sprintf("ответ Алисы %s: не удалось озвучить: %s", id8(id), detail)
			why = "Откат: все команды ответа переданы настоящему mediad, он говорит сам."
		case "file-start":
			msg = "звонок: запуск " + path.Base(detail) + " (плеер " + id8(id) + ")"
			why = "Платформа просит сыграть системный звук (таймер, будильник). Шим раскодирует файл и будет играть его по кругу, пока не придёт release; mediad команду не получил."
			if !strings.HasPrefix(detail, sound.SoundsDir) {
				msg, why = "звонок: "+detail, "Повторная стартовая команда того же плеера: звонок уже идёт, второй звук не нужен."
			}
		case "file-once":
			msg = "системный звук " + path.Base(detail) + " (плеер " + id8(id) + "): играет mediad"
			why = "Одноразовый звук (после «Алиса», щелчок громкости): он играет один раз, и конец сообщает сам плеер, а release платформа не присылает. Шим по кругу играет только звонки (поле 1.50 = 0), поэтому команда передана mediad."
		case "file-playing":
			msg = "звонок: пошёл звук"
			why = "Файл раскодирован и первые отсчёты ушли на динамик; платформа получила PLAYING. " + detail + "."
		case "file-release":
			msg = "звонок: плеер " + id8(id) + " освобождён"
			why = "Платформа остановила звонок (нажали кнопку, сказали «стоп» или время вышло): звук оборван, платформа получила ENDED."
		case "file-failed":
			lvl = dbg.Warn
			msg = fmt.Sprintf("звонок %s: не удалось сыграть: %s", id8(id), detail)
			why = "Если звука ещё не было, стартовая команда передана настоящему mediad и он звонит сам; если звук уже шёл, звонок считается законченным."
		case "handoff":
			msg = fmt.Sprintf("плеер %s: %s", id8(id), strings.TrimPrefix(detail, "шим останавливается: "))
			why = "Шим выключается и отдаёт плееры mediad: музыка встаёт на паузу с текущей позицией (команда «продолжи» запустит трек у mediad), ответ Алисы обрывается, звонок продолжается у mediad (ему передана стартовая команда)."
		case "failed":
			lvl = dbg.Warn
			msg = fmt.Sprintf("плеер %s: поток не открылся: %s", id8(id), detail)
			why = "Откат: команда play передаётся настоящему mediad, он сыграет трек сам."
		default:
			msg = kind + " " + detail
		}
		hub.Log(lvl, "player", msg, why, map[string]any{"player": id8(id), "mono_s": float64(clock.Mono()) / 1e9})
	}
}

// debugIntercept пишет в журнал команду, которую шим перехватил (настоящему mediad она не передана).
func debugIntercept(hub *dbg.Hub, connID uint64, cmd quasar.AudioCommand) {
	ci := dbg.InfoOfCommand(cmd)
	var msg, why string
	switch ci.Kind {
	case quasar.CmdPrefetch:
		msg, why = "перехвачена предзагрузка "+ci.Track.Title, "Команда не передана mediad: трек открывает шим."
	case quasar.CmdPlay:
		msg, why = "перехвачен запуск «"+ci.Track.Artist+" — "+ci.Track.Title+"»", "Команда не передана mediad: играет шим. Если поток не откроется, команда будет передана mediad."
	case quasar.CmdPause:
		msg, why = "перехвачена пауза плеера "+ci.Player.ID[:min(8, len(ci.Player.ID))], "Плеер ведёт шим, поэтому пауза выполнена им."
	case quasar.CmdCrossfade:
		msg, why = "перехвачена настройка кроссфейда", "Параметры перехода между треками запомнены, в событиях появятся поля 35 и 46."
	case quasar.CmdRelease:
		msg, why = "перехвачено освобождение плеера "+ci.Player.ID[:min(8, len(ci.Player.ID))], "Шим закрыл свой плеер."
	case quasar.CmdFileStart:
		msg, why = "перехвачен запуск звукового файла "+path.Base(ci.File), "Звонок таймера или будильника: играет шим, команда не передана mediad. Если звук не пойдёт, она будет передана mediad."
	default:
		msg, why = "перехвачена команда "+ci.Kind.String()+" ("+ci.Reason+")", "Команда касалась плеера шима и mediad не передана."
	}
	switch ci.Kind {
	case quasar.CmdVoiceStart:
		msg, why = "перехвачено начало ответа Алисы", "Голос играет шим: команда не передана mediad."
	case quasar.CmdVoiceChunk:
		msg, why = fmt.Sprintf("перехвачен фрагмент голоса, %d Б", ci.Size), "Сжатый звук (Opus) уходит в декодер шима."
	case quasar.CmdVoiceEOS:
		msg, why = "перехвачен конец данных ответа", "Все фрагменты ответа получены; шим доиграет и сообщит платформе."
	}
	d := ci.Data()
	d["conn"] = connID
	d["mono_s"] = float64(clock.Mono()) / 1e9 // CLOCK_MONOTONIC: сверка с trigger_time из pcmwatch
	lvl := dbg.Info
	if ci.Kind == quasar.CmdVoiceChunk {
		lvl = dbg.Trace
	}
	hub.Log(lvl, "player", msg, why, d)
}

// debugEvent пишет в журнал событие, которое шим сам отправил платформе.
func debugEvent(hub *dbg.Hub, body []byte) {
	ei := dbg.InfoOfEvent(body)
	msg, why := ei.Explain(true)
	hub.Log(ei.Level(), "player", "→платформа: "+msg, why, ei.Data())
}

// debugState — снимок для страницы: режим, соединения, плееры автомата и треки движка.
func debugState(p *shim.Proxy, m *player.Machine, eng *music.Engine, mode string) func() any {
	return func() any {
		st := map[string]any{"connections": p.NumConns()}
		if m == nil {
			st["mode"] = map[string]any{"режим": "прозрачный посредник: кадры 1:1, музыку играет mediad"}
			return st
		}
		st["mode"] = map[string]any{"режим": mode}
		st["players"] = m.Snapshot()
		st["engine"] = map[string]any{"треки": eng.Snapshot()}
		return st
	}
}

// watchAliced подключает к журналу диалог Алисы: что услышала, думает ли, говорит ли. Только чтение (как omd observe).
func watchAliced(ctx context.Context, hub *dbg.Hub) {
	o := &obs.Observer{
		Targets: []obs.Target{{Name: "aliced", Addr: "127.0.0.1:9875"}},
		Opt:     obs.Opt{},
		OnLine: func(who, text string) {
			lvl, why := dbg.Info, ""
			switch {
			case strings.HasPrefix(text, "AliceState"):
				switch {
				case strings.Contains(text, "LISTENING") && strings.Contains(text, "text="):
					why = "Алиса слушает; text — то, что она распознала (final — фраза закончена и пойдёт в облако)."
				case strings.Contains(text, "LISTENING"):
					why = "Алиса услышала «Алиса» и слушает команду."
				case strings.Contains(text, "BUSY"):
					why = "Фраза ушла в облако, Алиса думает; когда ответ придёт, в нём будут директивы (что делать и что сказать)."
				case strings.Contains(text, "SPEAKING"):
					why = "Алиса говорит: звук идёт через настоящий mediad."
				case strings.Contains(text, "IDLE"):
					why = "Диалог закончен."
				}
			case strings.HasPrefix(text, "Remoting"):
				lvl = dbg.Trace
			case strings.Contains(text, "нет соединения"), strings.Contains(text, "закрыто"):
				lvl = dbg.Warn
			}
			hub.Log(lvl, "aliced", text, why, nil)
		},
	}
	go o.Run(ctx)
}

// startDebug запускает отладочный порт и возвращает журнал. Адрес без хоста («:6701») слушает на всех интерфейсах.
func startDebug(ctx context.Context, addr string, cors bool, hub *dbg.Hub, state func() any, logf func(string, ...any)) {
	srv := &dbg.Server{Hub: hub, CORS: cors, State: state}
	go func() {
		if err := srv.Listen(ctx, addr); err != nil {
			logf("отладочный порт %s: %v", addr, err)
		}
	}()
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" {
		host = "адрес-колонки"
	}
	logf("отладочный порт: http://%s:%s/ (только чтение; CORS %v)", host, port, cors)
}
