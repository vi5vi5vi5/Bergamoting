package shim

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"openmediad/internal/quasar"
	"openmediad/internal/wire"
)

// Границы корзин гистограммы задержки шима (кадр пришёл → записан получателю).
var latBounds = [...]time.Duration{
	50 * time.Microsecond, 100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	50 * time.Millisecond, time.Second,
}

type statKey struct {
	dir  Dir
	kind string
}

type counter struct {
	frames, bytes uint64
	max           time.Duration
	hist          [len(latBounds) + 1]uint64
}

// Stats — сводка по проходящим кадрам: тип, число, объём, задержка шима. Подключается как Proxy.Tap.
type Stats struct {
	mu sync.Mutex
	m  map[statKey]*counter
}

// KindOf — имя типа кадра; у команд плееру уточняется назначением (voice-chunk, play, ...).
func KindOf(frame []byte) string {
	m, err := wire.Unwrap(frame)
	if err != nil {
		return "без-конверта"
	}
	if m.Type == wire.TypeAudioCommand {
		return m.Type.String() + "/" + quasar.AudioCommand(m.Body).Classify().String()
	}
	return m.Type.String()
}

func (s *Stats) Tap(c *Conn, d Dir, frame []byte, lat time.Duration) {
	k := statKey{d, KindOf(frame)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[statKey]*counter{}
	}
	ct := s.m[k]
	if ct == nil {
		ct = &counter{}
		s.m[k] = ct
	}
	ct.frames++
	ct.bytes += uint64(len(frame)) + 4
	if lat > ct.max {
		ct.max = lat
	}
	i := 0
	for i < len(latBounds) && lat >= latBounds[i] {
		i++
	}
	ct.hist[i]++
}

// quantile — верхняя граница корзины, в которую попадает доля q кадров.
func (c *counter) quantile(q float64) string {
	need := uint64(float64(c.frames)*q + 0.999999)
	if need == 0 {
		need = 1
	}
	var acc uint64
	for i, n := range c.hist {
		acc += n
		if acc >= need {
			if i == len(latBounds) {
				return ">" + latBounds[len(latBounds)-1].String()
			}
			return "<" + latBounds[i].String()
		}
	}
	return "?"
}

// Report печатает сводку и при reset обнуляет счётчики (окно наблюдения). Пустую сводку не печатает.
func (s *Stats) Report(w io.Writer, label string, reset bool) {
	s.mu.Lock()
	keys := make([]statKey, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dir != keys[j].dir {
			return keys[i].dir < keys[j].dir
		}
		return keys[i].kind < keys[j].kind
	})
	var b strings.Builder
	for _, k := range keys {
		c := s.m[k]
		fmt.Fprintf(&b, "  %s %-26s %6d кадр. %9d Б  задержка шима: p50 %s p99 %s макс %s\n",
			k.dir, k.kind, c.frames, c.bytes, c.quantile(0.5), c.quantile(0.99), c.max.Round(time.Microsecond))
	}
	if reset {
		clear(s.m)
	}
	s.mu.Unlock()
	if b.Len() > 0 {
		fmt.Fprintf(w, "%s\n%s", label, b.String())
	}
}
