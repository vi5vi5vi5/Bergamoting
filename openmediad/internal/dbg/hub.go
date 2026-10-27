// Package dbg — отладочный порт шима: кольцевой журнал с пояснениями, потоковая выдача (SSE) и веб-страница.
//
// Журнал нужен человеку, который развивает проект: что за кадр прошёл по шине, что с ним сделал шим и почему.
// Секреты (ключи расшифровки потоков, подписи URL, тела директив с идентификаторами запросов) в журнал не попадают:
// explain.go отдаёт только безопасные поля (id трека, название, хост, размеры).
package dbg

import (
	"fmt"
	"sync"
	"time"
)

// Уровни записи.
const (
	Trace = "trace" // фон: прогресс плеера раз в секунду, служебные кадры
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// Entry — одна запись журнала.
type Entry struct {
	ID   uint64         `json:"id"`
	T    int64          `json:"t"`              // unix, миллисекунды
	Lvl  string         `json:"lvl"`            // trace | info | warn | error
	Src  string         `json:"src"`            // откуда: shim, audioclient, player, music, aliced, system
	Msg  string         `json:"msg"`            // коротко, что произошло
	Why  string         `json:"why,omitempty"`  // пояснение простыми словами: что это значит и что сделал шим
	Data map[string]any `json:"data,omitempty"` // детали без секретов
}

// Hub — кольцевой журнал на последние Cap записей и подписчики на новые.
type Hub struct {
	Cap int // сколько записей держать (по умолчанию 4000)

	mu     sync.Mutex
	ring   []Entry
	head   int // индекс самой старой записи, когда кольцо полно
	next   uint64
	subs   map[chan Entry]struct{}
	Start  time.Time
	counts map[string]uint64 // записей по источникам за всё время
}

func (h *Hub) init() {
	if h.ring != nil {
		return
	}
	if h.Cap <= 0 {
		h.Cap = 4000
	}
	h.ring = make([]Entry, 0, h.Cap)
	h.subs = map[chan Entry]struct{}{}
	h.counts = map[string]uint64{}
	if h.Start.IsZero() {
		h.Start = time.Now()
	}
}

// Log добавляет запись и рассылает подписчикам. Медленный подписчик пропускает записи, а не тормозит шим.
func (h *Hub) Log(lvl, src, msg, why string, data map[string]any) {
	h.mu.Lock()
	h.init()
	h.next++
	e := Entry{ID: h.next, T: time.Now().UnixMilli(), Lvl: lvl, Src: src, Msg: msg, Why: why, Data: data}
	if len(h.ring) < h.Cap {
		h.ring = append(h.ring, e)
	} else {
		h.ring[h.head] = e
		h.head = (h.head + 1) % h.Cap
	}
	h.counts[src]++
	subs := make([]chan Entry, 0, len(h.subs))
	for c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- e:
		default: // подписчик не успевает: запись ему не достанется, он увидит разрыв по номерам ID
		}
	}
}

// Logf — запись уровня info из printf-подобного журнала остальных пакетов (без пояснения).
func (h *Hub) Logf(src string) func(string, ...any) {
	return func(format string, a ...any) {
		h.Log(Info, src, fmt.Sprintf(format, a...), "", nil)
	}
}

// Since возвращает записи с ID больше after (по порядку), не более limit самых свежих (0 — все).
func (h *Hub) Since(after uint64, limit int) []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	n := len(h.ring)
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		e := h.ring[(h.head+i)%n]
		if e.ID > after {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe возвращает канал новых записей и функцию отписки.
func (h *Hub) Subscribe() (<-chan Entry, func()) {
	h.mu.Lock()
	h.init()
	c := make(chan Entry, 256)
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c, func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}
}

// Subscribers — число подключённых слушателей.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Counts — сколько записей накоплено по источникам (для страницы состояния).
func (h *Hub) Counts() map[string]uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	out := make(map[string]uint64, len(h.counts))
	for k, v := range h.counts {
		out[k] = v
	}
	return out
}
