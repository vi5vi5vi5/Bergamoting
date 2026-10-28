package main

import (
	"context"
	"sync"
	"time"

	"openmediad/internal/dbg"
	"openmediad/internal/iohub"
	"openmediad/internal/spectrum"
)

// specDelay — на сколько метка времени кадра спектра опережает момент, когда кадр отправлен: через столько после передачи
// звука aplay он прозвучит (труба 64 КБ ≈ 340 мс и буфер смесителя). У штатного mediad кадр уходит сразу, а в поле 1 стоит
// время, когда звук будет слышен: приход − метка ≈ −0,29 с во всех записях штатной музыки и голоса. Метка в прошлом
// (наш первый вариант: кадр держали 300 мс и ставили время расчёта, приход − метка = +0,30 с) — платформа не рисует анимацию.
// Значение оценочное; флаг -spectrum-delay позволяет подобрать его глазами.
const specDelay = 300 * time.Millisecond

// frameSender — куда уходят сообщения спектра (iohub.Client; в тестах подмена).
type frameSender interface{ Send(msg []byte) bool }

// specSender берёт события спектра движка и отправляет их iohub по порядку. Кадр уходит сразу, с меткой «когда прозвучит»
// (время расчёта + delay); остановка потока (6:2) уходит через delay, когда последний кадр уже прозвучал, как у mediad.
type specSender struct {
	cli   frameSender
	delay time.Duration
	hub   *dbg.Hub

	mu     sync.Mutex
	q      []queued
	wake   chan struct{}
	frames uint64
}

type queued struct {
	at time.Time // когда отправить
	ev spectrum.Event
}

func newSpecSender(cli frameSender, delay time.Duration, hub *dbg.Hub) *specSender {
	return &specSender{cli: cli, delay: delay, hub: hub, wake: make(chan struct{}, 1)}
}

// Add ставит событие в очередь (копирует кадр: движок переиспользует срез).
func (s *specSender) Add(ev spectrum.Event) {
	if ev.DB != nil {
		ev.DB = append([]float32(nil), ev.DB...)
	}
	at := time.Now()
	switch ev.Kind {
	case iohub.KindData:
		ev.At = ev.At.Add(s.delay) // метка кадра — момент, когда звук прозвучит; сам кадр уходит сразу
	case iohub.KindStop:
		at = at.Add(s.delay) // остановка — после того, как прозвучал последний кадр
	}
	s.mu.Lock()
	if len(s.q) > 512 { // iohub не принимает: не копить
		s.q = s.q[1:]
	}
	s.q = append(s.q, queued{at, ev})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *specSender) Run(ctx context.Context) {
	for {
		s.mu.Lock()
		var head *queued
		if len(s.q) > 0 {
			head = &s.q[0]
		}
		var wait time.Duration
		if head != nil {
			wait = time.Until(head.at)
		}
		s.mu.Unlock()
		switch {
		case head == nil:
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
		case wait > 0:
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		default:
			s.mu.Lock()
			q := s.q[0]
			s.q = s.q[1:]
			s.mu.Unlock()
			s.send(q.ev)
		}
	}
}

func (s *specSender) send(ev spectrum.Event) {
	switch ev.Kind {
	case iohub.KindData:
		s.frames++
		s.cli.Send(iohub.Data(uint64(ev.At.UnixNano()), ev.DB, ev.Stream, uint64(ev.Source)))
	default:
		s.cli.Send(iohub.Control(uint64(ev.Kind), ev.Stream, uint64(ev.Source)))
		if s.hub != nil {
			names := map[int]string{iohub.KindStart: "начат", iohub.KindStop: "остановлен", iohub.KindResume: "продолжен"}
			s.hub.Log(dbg.Info, "player", "визуализация: поток спектра "+names[ev.Kind], "Для дисплея и светодиодов: сервису iohub отправлено служебное сообщение потока спектра (как это делает mediad). Кадры спектра идут 25 раз в секунду и в журнал не пишутся.",
				map[string]any{"stream": ev.Stream, "кадров отправлено": s.frames})
		}
	}
}
