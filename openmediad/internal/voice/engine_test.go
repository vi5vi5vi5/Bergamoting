package voice

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openmediad/internal/spectrum"
)

// fakeSink собирает PCM; gate (если задан) блокирует запись, имитируя медленный aplay, пока не открыт или не вызван Abort.
type fakeSink struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	aborted  atomic.Bool
	finished atomic.Bool
	gate     chan struct{}
}

func (f *fakeSink) Write(p []byte) (int, error) {
	if f.gate != nil {
		<-f.gate
		if f.aborted.Load() {
			return 0, errors.New("aplay оборван")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}
func (f *fakeSink) Abort() {
	if f.aborted.CompareAndSwap(false, true) && f.gate != nil {
		close(f.gate)
	}
}
func (f *fakeSink) Finish() { f.finished.Store(true) }
func (f *fakeSink) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.buf.Bytes()...)
}

// fakeDec «декодирует» чанк: на каждый байт отдаёт 8 байт PCM (96 000 Б = секунда звука при 96 Б/мс не нужна: хватает пропорции).
// Чанк, начинающийся с «BAD», вызывает ошибку. Перед PCM отдаёт «pre-skip» в tap, как настоящий декодер.
type fakeDec struct {
	tap    func([]byte)
	resets int
}

func (d *fakeDec) Reset() { d.resets++ }
func (d *fakeDec) Write(chunk []byte, sink func([]byte) error) error {
	if bytes.HasPrefix(chunk, []byte("BAD")) {
		return errors.New("битый чанк")
	}
	if d.tap != nil {
		d.tap(make([]byte, 624)) // 312 отсчётов pre-skip
	}
	pcm := make([]byte, 8*len(chunk))
	for i := range pcm {
		pcm[i] = chunk[i/8]
	}
	return sink(pcm)
}

type rig struct {
	e       *Engine
	mu      sync.Mutex
	sinks   []*fakeSink
	decs    []*fakeDec
	events  chan string
	spec    []spectrum.Event
	sink    func() *fakeSink
	sinkErr error
}

func newRig(t *testing.T) *rig {
	r := &rig{events: make(chan string, 64)}
	r.sink = func() *fakeSink { return &fakeSink{} }
	r.e = &Engine{
		NewSink: func() (Sink, error) {
			if r.sinkErr != nil {
				return nil, r.sinkErr
			}
			s := r.sink()
			r.mu.Lock()
			r.sinks = append(r.sinks, s)
			r.mu.Unlock()
			return s, nil
		},
		NewDecoder: func(tap func([]byte)) (Decoder, error) {
			d := &fakeDec{tap: tap}
			r.mu.Lock()
			r.decs = append(r.decs, d)
			r.mu.Unlock()
			return d, nil
		},
		Logf:      func(f string, a ...any) { t.Logf(f, a...) },
		OnPlaying: func(id string) { r.events <- "playing " + id },
		OnEnded:   func(id string) { r.events <- "ended " + id },
		OnFailed:  func(id string, err error) { r.events <- "failed " + id + ": " + err.Error() },
		OnSpectrum: func(ev spectrum.Event) {
			r.mu.Lock()
			ev.DB = append([]float32(nil), ev.DB...)
			r.spec = append(r.spec, ev)
			r.mu.Unlock()
		},
	}
	t.Cleanup(r.e.Shutdown)
	return r
}

func (r *rig) next(t *testing.T) string {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("не дождались события движка")
		return ""
	}
}

func (r *rig) noEvent(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev := <-r.events:
		t.Fatalf("неожиданное событие: %s", ev)
	case <-time.After(d):
	}
}

func (r *rig) sinkAt(i int) *fakeSink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sinks[i]
}

func TestPlaysChunksInOrderThenEnds(t *testing.T) {
	r := newRig(t)
	r.e.Start("v1")
	r.e.Chunk("v1", []byte("AB"))
	if ev := r.next(t); ev != "playing v1" {
		t.Fatalf("первым событием ждали playing, пришло %q", ev)
	}
	r.e.Chunk("v1", []byte("CD"))
	r.e.EOS("v1")
	if ev := r.next(t); ev != "ended v1" {
		t.Fatalf("затем ended, пришло %q", ev)
	}
	got := r.sinkAt(0).bytes()
	want := append(bytes.Repeat([]byte("A"), 8), append(bytes.Repeat([]byte("B"), 8), append(bytes.Repeat([]byte("C"), 8), bytes.Repeat([]byte("D"), 8)...)...)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("PCM в порядке чанков нарушен: %q", got)
	}
	if !r.sinkAt(0).finished.Load() || r.sinkAt(0).aborted.Load() {
		t.Fatal("в конце ответа aplay должен доиграть (Finish), а не обрываться")
	}
	if p := r.e.Pos("v1"); p != 0 {
		t.Fatalf("после конца ответа позиция сбрасывается вместе с сессией, а она %d", p)
	}
	r.noEvent(t, 100*time.Millisecond) // playing — один раз за ответ
}

func TestPositionCountsPlayedBytes(t *testing.T) {
	r := newRig(t)
	r.sink = func() *fakeSink { return &fakeSink{gate: make(chan struct{})} }
	r.e.Start("v1")
	r.e.Chunk("v1", make([]byte, 120)) // 960 байт PCM = 10 мс
	time.Sleep(50 * time.Millisecond)
	r.sinkAt(0).gate <- struct{}{} // выпустить первую запись
	if ev := r.next(t); ev != "playing v1" {
		t.Fatalf("получено %q", ev)
	}
	time.Sleep(40 * time.Millisecond) // позиция не может опережать часы: через 40 мс «прошло» уже больше, чем «записано»
	if p := r.e.Pos("v1"); p != 10 {
		t.Fatalf("позиция %d мс, ждали 10 (960 Б при 96 Б/мс)", p)
	}
	r.e.Abort("v1")
}

func TestAbortStopsSoundAndSkipsEnded(t *testing.T) {
	r := newRig(t)
	r.sink = func() *fakeSink { return &fakeSink{gate: make(chan struct{})} }
	r.e.Start("v1")
	r.e.Chunk("v1", []byte("AB"))
	time.Sleep(50 * time.Millisecond) // запись заблокирована «медленным aplay»
	r.e.Abort("v1")
	time.Sleep(100 * time.Millisecond)
	if !r.sinkAt(0).aborted.Load() {
		t.Fatal("Abort должен оборвать aplay")
	}
	r.noEvent(t, 150*time.Millisecond) // ни ended, ни failed: прерывание — не конец и не ошибка
	r.e.Chunk("v1", []byte("more"))    // после Abort чанки игнорируются
	r.noEvent(t, 50*time.Millisecond)
}

func TestNewStartDisplacesOldReply(t *testing.T) {
	r := newRig(t)
	r.sink = func() *fakeSink { return &fakeSink{gate: make(chan struct{})} }
	r.e.Start("v1")
	r.e.Chunk("v1", []byte("AB"))
	time.Sleep(50 * time.Millisecond)
	r.sink = func() *fakeSink { return &fakeSink{} }
	r.e.Start("v2")
	time.Sleep(100 * time.Millisecond)
	if !r.sinkAt(0).aborted.Load() {
		t.Fatal("старый ответ должен быть оборван новым")
	}
	r.e.Chunk("v2", []byte("XY"))
	r.e.EOS("v2")
	for _, want := range []string{"playing v2", "ended v2"} {
		if ev := r.next(t); ev != want {
			t.Fatalf("ждали %q, пришло %q", want, ev)
		}
	}
	r.mu.Lock()
	n := len(r.decs)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("декодер должен быть один на всю жизнь движка, создано %d", n)
	}
}

func TestSinkFailureIsReported(t *testing.T) {
	r := newRig(t)
	r.sinkErr = errors.New("нет aplay")
	r.e.Start("v1")
	if ev := r.next(t); !strings.HasPrefix(ev, "failed v1") {
		t.Fatalf("получено %q", ev)
	}
}

func TestBadFirstChunkFailsBeforeAudio(t *testing.T) {
	r := newRig(t)
	r.e.Start("v1")
	r.e.Chunk("v1", []byte("BAD data"))
	if ev := r.next(t); !strings.HasPrefix(ev, "failed v1") {
		t.Fatalf("получено %q", ev)
	}
	time.Sleep(50 * time.Millisecond)
	if !r.sinkAt(0).aborted.Load() {
		t.Fatal("после ошибки до звука вывод должен быть закрыт")
	}
}

func TestBadChunkInTheMiddleIsSkipped(t *testing.T) {
	r := newRig(t)
	r.e.Start("v1")
	r.e.Chunk("v1", []byte("AB"))
	if ev := r.next(t); ev != "playing v1" {
		t.Fatalf("получено %q", ev)
	}
	r.e.Chunk("v1", []byte("BAD"))
	r.e.Chunk("v1", []byte("CD"))
	r.e.EOS("v1")
	if ev := r.next(t); ev != "ended v1" {
		t.Fatalf("после битого чанка посреди ответа ответ должен дойти до конца, пришло %q", ev)
	}
	if got := r.sinkAt(0).bytes(); len(got) != 8*4 {
		t.Fatalf("сыграно %d байт, ждали 32 (два хороших чанка)", len(got))
	}
}

func TestSpectrumStartDataStopWithPreSkip(t *testing.T) {
	r := newRig(t)
	r.e.Start("v1")
	r.e.Chunk("v1", make([]byte, 6000)) // 48000 байт PCM = 24000 отсчётов + 312 pre-skip в tap → 12 интервалов по 1920
	r.e.EOS("v1")
	r.next(t)
	r.next(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.spec) < 3 || r.spec[0].Kind != spectrum.KindStart || r.spec[len(r.spec)-1].Kind != spectrum.KindStop {
		t.Fatalf("поток спектра должен начинаться Start и кончаться Stop, события: %d", len(r.spec))
	}
	data := 0
	for _, ev := range r.spec {
		if ev.Source != spectrum.SourceVoice {
			t.Fatal("спектр голоса помечается источником «голос» (46.8 = 1)")
		}
		if ev.Stream != r.spec[0].Stream {
			t.Fatal("у ответа один идентификатор потока")
		}
		if ev.Kind == spectrum.KindData {
			data++
		}
	}
	if want := (312 + 24000) / spectrum.FrameLen; data != want {
		t.Fatalf("кадров спектра %d, ждали %d (pre-skip идёт в спектр)", data, want)
	}
}

func TestChunkForUnknownReplyIgnored(t *testing.T) {
	r := newRig(t)
	r.e.Chunk("nope", []byte("AB"))
	r.e.EOS("nope")
	r.noEvent(t, 100*time.Millisecond)
}

func TestShutdownStopsAndRefusesNewReplies(t *testing.T) {
	r := newRig(t)
	r.sink = func() *fakeSink { return &fakeSink{gate: make(chan struct{})} }
	r.e.Start("v1")
	r.e.Chunk("v1", []byte("AB"))
	time.Sleep(50 * time.Millisecond)
	r.e.Shutdown()
	time.Sleep(100 * time.Millisecond)
	if !r.sinkAt(0).aborted.Load() {
		t.Fatal("Shutdown обрывает идущий ответ")
	}
	r.e.Start("v2")
	r.noEvent(t, 100*time.Millisecond)
	r.mu.Lock()
	n := len(r.sinks)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("после Shutdown новый вывод не запускается, а запущено %d", n)
	}
}

// Позиция идёт по часам, а не по байтам, записанным в трубу: иначе progress убегал бы вперёд на размер буферов.
func TestPositionDoesNotRunAheadOfClock(t *testing.T) {
	r := newRig(t)
	r.e.Start("v1")
	r.e.Chunk("v1", make([]byte, 12500)) // 100 000 Б = больше секунды звука записывается мгновенно
	if ev := r.next(t); ev != "playing v1" {
		t.Fatalf("получено %q", ev)
	}
	time.Sleep(200 * time.Millisecond)
	p := r.e.Pos("v1")
	if p < 150 || p > 400 {
		t.Fatalf("через 200 мс после начала позиция %d мс: ждали около 200 (по часам), а не 1000+ (по записанным байтам)", p)
	}
	r.e.Abort("v1")
}
