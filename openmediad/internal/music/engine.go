package music

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"openmediad/internal/player"
	"openmediad/internal/sink"
)

const (
	pcmBytesPerMs = 192                  // 48 кГц × 2 канала × 2 байта / 1000
	ringBytes     = 4000 * pcmBytesPerMs // запас PCM между декодером и выводом: 4 с
	historyBytes  = 1500 * pcmBytesPerMs // история для возврата звука при паузе: 1,5 с
	chunkBytes    = 50 * pcmBytesPerMs   // порция вывода: 50 мс
	openLevel     = 300 * pcmBytesPerMs  // «поток открыт», когда готовы 300 мс звука
	skipLimit     = 256 << 10            // дальше этого от текущей позиции поток открывается заново (Range)
	fadeInBytes   = 150 * pcmBytesPerMs  // нарастание громкости при старте вывода: 150 мс
	maxRetries    = 2                    // повторов чтения кадра с другим зеркалом
	ductRampPerCh = 0.25                 // шаг сглаживания громкости на порцию вывода
)

// PCMSink — приёмник звука: в бою aplay, в тестах запись в память.
type PCMSink interface {
	io.Writer
	Abort()  // оборвать сразу (пауза, смена трека)
	Finish() // доиграть остаток и закрыть
}

type aplaySink struct{ p *sink.Player }

func (a aplaySink) Write(b []byte) (int, error) { return a.p.Write(b) }
func (a aplaySink) Abort()                      { a.p.Abort(2 * time.Second) }
func (a aplaySink) Finish()                     { a.p.Finish(5 * time.Second) }

// NewAplaySink запускает aplay на общий смеситель колонки (стерео 48 кГц).
func NewAplaySink() (PCMSink, error) {
	p, err := sink.Start(sink.Config{Channels: 2})
	if err != nil {
		return nil, err
	}
	return aplaySink{p}, nil
}

// Engine — «настоящий звук» для player.Machine: скачивание, расшифровка, декодирование и вывод треков.
// Реализует player.Backend. Сообщения о результатах уходят в On*.
type Engine struct {
	Client  *http.Client
	NewDec  DecoderFactory
	NewSink func() (PCMSink, error)
	Logf    func(string, ...any)
	// NoRewind отключает возврат звука, пропавшего в буферах aplay при паузе (для тестов с мгновенным приёмником).
	NoRewind bool
	// Now — часы для оценки того, что уже прозвучало (по умолчанию time.Now); подменяется в тестах.
	Now func() time.Time
	// NoFadeIn отключает нарастание громкости в начале вывода (для тестов, которые читают отсчёты как метки).
	NoFadeIn bool

	// OnSpectrum (необязательный) получает кадры спектра и смены состояния потока для визуализации.
	// Вызывается из цикла вывода: обработчик должен быстро возвращаться.
	OnSpectrum func(SpectrumEvent)

	OnOpened   func(id string, durMs uint64)
	OnFailed   func(id string, err error)
	OnFinished func(id string)

	mu       sync.Mutex
	tracks   map[string]*track
	gainBits atomic.Uint64
	inited   bool
}

func (e *Engine) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

func (e *Engine) init() {
	if e.inited {
		return
	}
	e.inited = true
	e.tracks = map[string]*track{}
	e.gainBits.Store(math.Float64bits(1))
	if e.Client == nil {
		e.Client = NewClient()
	}
	if e.NewDec == nil {
		e.NewDec = NewGstDecoder
	}
	if e.NewSink == nil {
		e.NewSink = NewAplaySink
	}
}

type session struct {
	cancel context.CancelFunc
	ring   *ring
	dec    Decoder
}

type track struct {
	e   *Engine
	id  string
	src player.Track
	got atomic.Uint64
	tot atomic.Int64

	mu      sync.Mutex
	idx     *Index
	sess    *session
	baseMs  uint64
	played  atomic.Uint64 // байт PCM, переданных выводу с начала сессии
	outGen  int           // поколение цикла вывода: смена останавливает старый цикл
	playing bool
	opened  bool
	closed  bool
	spec    uint64 // идентификатор потока спектра (0 — ещё не начат)
	// учёт текущего цикла вывода (для позиции и возврата звука при паузе)
	outAt     atomic.Int64  // unix-наносекунды первой записи цикла в aplay (0 — не играет)
	outPlayed atomic.Uint64 // played в начале цикла
	loopDone  chan struct{} // закрывается, когда последний запущенный цикл вывода завершился
	ctx       context.Context
	cancel    context.CancelFunc
}

// Warm готовит клиента HTTP заранее: чтение хранилища корневых сертификатов занимает десятки миллисекунд, и первое
// же обращение к движку (команда focus) не должно задерживать кадры шины.
func (e *Engine) Warm() {
	e.mu.Lock()
	e.init()
	e.mu.Unlock()
}

func (e *Engine) get(id string) *track {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	return e.tracks[id]
}

// Open начинает загрузку трека (prefetch или play): скачивание и декодирование идут сами, пока буфер не заполнится.
func (e *Engine) Open(id string, t player.Track, offsetMs uint64) {
	e.mu.Lock()
	e.init()
	if old := e.tracks[id]; old != nil {
		// Повторное открытие уже открытого трека (повторный prefetch): сообщаем об открытии ещё раз, иначе тот, кто
		// ждёт события, не узнает, что поток давно готов.
		old.mu.Lock()
		opened, dur := old.opened, uint64(0)
		if old.idx != nil {
			dur = old.idx.DurationMs
		}
		old.mu.Unlock()
		e.mu.Unlock()
		if opened && e.OnOpened != nil {
			go e.OnOpened(id, dur)
		}
		return
	}
	tr := &track{e: e, id: id, src: t}
	tr.ctx, tr.cancel = context.WithCancel(context.Background())
	e.tracks[id] = tr
	e.mu.Unlock()
	go tr.boot(offsetMs)
}

func (t *track) urls() []string {
	return append([]string{t.src.URL}, t.src.Alts...)
}

func (t *track) fail(err error) {
	t.mu.Lock()
	closed := t.closed
	opened := t.opened
	t.mu.Unlock()
	if closed {
		return
	}
	t.e.logf("трек %s (%s): %v", t.id[:min(8, len(t.id))], t.src.Title, err)
	if opened {
		// поток оборвался посреди трека: считаем, что трек закончился, очередь пойдёт дальше
		t.mu.Lock()
		s := t.sess
		t.mu.Unlock()
		if s != nil {
			s.ring.CloseWrite()
		}
		return
	}
	if t.e.OnFailed != nil {
		t.e.OnFailed(t.id, err)
	}
}

// boot открывает поток, читает moov и запускает первую сессию.
func (t *track) boot(startMs uint64) {
	st, err := Open(t.ctx, t.e.Client, t.urls(), t.src.Key, 0, &t.got)
	if err != nil {
		t.fail(fmt.Errorf("открытие потока: %w", err))
		return
	}
	idx, err := ReadMoov(st)
	if err != nil {
		st.Close()
		t.fail(err)
		return
	}
	t.tot.Store(st.Total)
	t.mu.Lock()
	t.idx = idx
	t.mu.Unlock()
	t.start(st, startMs)
}

// start создаёт сессию декодирования с позиции posMs. hdr — уже открытый поток сразу после moov (или nil).
func (t *track) start(hdr *Stream, posMs uint64) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		if hdr != nil {
			hdr.Close()
		}
		return
	}
	idx := t.idx
	i0 := idx.SampleAtMs(posMs)
	if posMs >= idx.DurationMs {
		i0 = idx.N()
	}
	dec, err := t.e.NewDec()
	if err != nil {
		t.mu.Unlock()
		if hdr != nil {
			hdr.Close()
		}
		t.fail(fmt.Errorf("запуск декодера: %w", err))
		return
	}
	ctx, cancel := context.WithCancel(t.ctx)
	s := &session{cancel: cancel, ring: newRing(ringBytes, historyBytes), dec: dec}
	t.sess = s
	t.baseMs = idx.StartMs(min(i0, idx.N()-1))
	t.played.Store(0)
	first := !t.opened
	t.mu.Unlock()

	go t.pump(s)
	go t.feed(ctx, s, hdr, i0)
	if first {
		go func() {
			if s.ring.WaitLevel(openLevel) {
				t.mu.Lock()
				t.opened = true
				closed := t.closed
				t.mu.Unlock()
				if !closed && t.e.OnOpened != nil {
					t.e.OnOpened(t.id, idx.DurationMs)
				}
			}
		}()
	}
}

// pump переливает PCM из декодера в буфер.
func (t *track) pump(s *session) {
	n, err := io.Copy(s.ring, s.dec)
	if err != nil && !errors.Is(err, errStopped) {
		t.e.logf("трек %s: чтение PCM: %v", t.id[:min(8, len(t.id))], err)
	}
	if n == 0 && !errors.Is(err, errStopped) {
		tail := ""
		if g, ok := s.dec.(interface{ Tail() string }); ok {
			tail = g.Tail()
		}
		t.mu.Lock()
		cur := t.sess == s && !t.closed
		t.mu.Unlock()
		if cur {
			t.fail(fmt.Errorf("декодер не выдал звук: %.300s", tail))
		}
	}
	s.ring.CloseWrite()
}

// feed режет поток на кадры AAC по таблице moov и отдаёт декодеру в обёртке ADTS.
func (t *track) feed(ctx context.Context, s *session, hdr *Stream, i0 int) {
	defer func() {
		s.dec.CloseWrite()
	}()
	t.mu.Lock()
	idx := t.idx
	t.mu.Unlock()
	st, pos := hdr, int64(-1)
	if hdr != nil {
		pos = int64(idx.MoovEnd)
	}
	defer func() {
		if st != nil {
			st.Close()
		}
	}()
	var maxSz uint32
	for _, z := range idx.Size {
		maxSz = max(maxSz, z)
	}
	buf := make([]byte, 7+int(maxSz))
	for i := i0; i < idx.N(); i++ {
		off, size := int64(idx.Off[i]), int(idx.Size[i])
		for attempt := 0; ; attempt++ {
			if ctx.Err() != nil {
				return
			}
			var err error
			if st == nil || off < pos || off-pos > skipLimit {
				if st != nil {
					st.Close()
					st = nil
				}
				st, err = Open(ctx, t.e.Client, t.urls(), t.src.Key, off, &t.got)
				if err == nil {
					pos = off
				}
			} else if off > pos {
				if _, err = io.CopyN(io.Discard, st, off-pos); err == nil {
					pos = off
				}
			}
			if err == nil {
				_, err = io.ReadFull(st, buf[7:7+size])
			}
			if err == nil {
				pos += int64(size)
				break
			}
			if st != nil {
				st.Close()
				st = nil
			}
			if ctx.Err() != nil {
				return
			}
			if attempt >= maxRetries {
				t.fail(fmt.Errorf("чтение кадра %d: %w", i, err))
				return
			}
			t.e.logf("трек %s: кадр %d: %v — повтор", t.id[:min(8, len(t.id))], i, err)
			time.Sleep(200 * time.Millisecond)
		}
		h, err := ADTS(idx.ASC, size)
		if err != nil {
			t.fail(err)
			return
		}
		copy(buf, h[:])
		if _, err := s.dec.Write(buf[:7+size]); err != nil {
			return
		}
	}
}
