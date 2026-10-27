package sound

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSink — приёмник в памяти: копит записанное и «играет» в десять раз быстрее реального времени, чтобы запись не вращалась вхолостую.
type fakeSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	aborted atomic.Bool
	failAt  int // после стольких записанных байт Write возвращает ошибку (0 — никогда)
}

func (f *fakeSink) Write(p []byte) (int, error) {
	if f.aborted.Load() {
		return 0, errors.New("вывод закрыт")
	}
	f.mu.Lock()
	f.buf.Write(p)
	n := f.buf.Len()
	f.mu.Unlock()
	if f.failAt > 0 && n >= f.failAt {
		return 0, errors.New("aplay умер")
	}
	time.Sleep(time.Duration(len(p)/bytesPerMs) * time.Millisecond / 10)
	return len(p), nil
}

func (f *fakeSink) Abort() { f.aborted.Store(true) }

func (f *fakeSink) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.buf.Bytes()...)
}

// pattern — PCM из n байт с повторяющимся, но не кратным размеру записи узором: сбой на стыке проходов виден по смещению.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("не дождались: %s", what)
}

type rig struct {
	e        *Engine
	sinks    []*fakeSink
	decodes  atomic.Int32
	playing  chan [3]any
	failed   chan error
	mu       sync.Mutex
	newSinks int
}

func newRig(clip *Clip, decErr error) *rig {
	r := &rig{playing: make(chan [3]any, 8), failed: make(chan error, 8)}
	r.e = &Engine{
		NewSink: func() (Sink, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.newSinks++
			s := &fakeSink{}
			r.sinks = append(r.sinks, s)
			return s, nil
		},
		Decode: func(path string) (*Clip, error) {
			r.decodes.Add(1)
			if decErr != nil {
				return nil, decErr
			}
			return clip, nil
		},
		OnPlaying: func(id string, durMs, bitrate uint64) { r.playing <- [3]any{id, durMs, bitrate} },
		OnFailed:  func(id string, err error) { r.failed <- err },
	}
	return r
}

func (r *rig) sink(i int) *fakeSink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sinks[i]
}

func TestLoopsGaplesslyUntilStopped(t *testing.T) {
	pcm := pattern(20000) // три записи на проход: 8192 + 8192 + 3616
	r := newRig(&Clip{PCM: pcm, Bitrate: 64}, nil)
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	select {
	case p := <-r.playing:
		if p[0] != "a" || p[1] != uint64(20000/bytesPerMs) || p[2] != uint64(64) {
			t.Fatalf("OnPlaying = %v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("нет OnPlaying")
	}
	sk := r.sink(0)
	waitFor(t, "три прохода", func() bool { return len(sk.bytes()) > 3*len(pcm) })
	r.e.Stop("a")
	waitFor(t, "aplay оборван", sk.aborted.Load)
	got := sk.bytes()
	for i, b := range got {
		if b != pcm[i%len(pcm)] {
			t.Fatalf("байт %d = %d, ждали %d: проход начался не с начала файла", i, b, pcm[i%len(pcm)])
		}
	}
	n := len(got)
	time.Sleep(30 * time.Millisecond)
	if len(sk.bytes()) != n {
		t.Fatal("после Stop запись продолжается")
	}
	if len(r.playing) != 0 {
		t.Fatal("OnPlaying вызван повторно")
	}
	if p, el := r.e.Pos("a"), r.e.Elapsed("a"); p != 0 || el != 0 {
		t.Fatalf("после Stop позиция %d, прошло %d", p, el)
	}
}

func TestPositionAndElapsedFollowTheClockAndWrap(t *testing.T) {
	e := &Engine{sessions: map[string]*session{}}
	s := &session{e: e, id: "x"}
	e.sessions["x"] = s
	if e.Pos("x") != 0 || e.Elapsed("x") != 0 {
		t.Fatal("до первого звука позиция и время должны быть 0")
	}
	s.durMs.Store(9000)
	s.startAt.Store(time.Now().Add(-10 * time.Second).UnixNano())
	s.written.Store(60 * 1000 * bytesPerMs) // записано заведомо больше, чем прошло
	if el := e.Elapsed("x"); el < 9990 || el > 10100 {
		t.Fatalf("Elapsed = %d, ждали ≈10000", el)
	}
	if p := e.Pos("x"); p < 990 || p > 1100 { // 10 с по кругу в 9 с — позиция ≈ 1 с
		t.Fatalf("Pos = %d, ждали ≈1000 (круг 9000 мс)", p)
	}
	// aplay записан меньше, чем прошло по часам (буфер пуст): время не бежит впереди записанного
	s.written.Store(2500 * bytesPerMs)
	if el := e.Elapsed("x"); el != 2500 {
		t.Fatalf("Elapsed = %d, ждали 2500 (записано)", el)
	}
	if p := e.Pos("x"); p != 2500 {
		t.Fatalf("Pos = %d, ждали 2500", p)
	}
}

func TestDecodeFailureReportsAndNeverPlays(t *testing.T) {
	r := newRig(nil, errors.New("gst упал"))
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	select {
	case err := <-r.failed:
		if !strings.Contains(err.Error(), "gst упал") || !strings.Contains(err.Error(), "раскодирование") {
			t.Fatalf("ошибка = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("нет OnFailed")
	}
	waitFor(t, "aplay остановлен", r.sink(0).aborted.Load)
	if len(r.playing) != 0 {
		t.Fatal("OnPlaying после сбоя")
	}
	if r.e.get("a") != nil {
		t.Fatal("сбойный звонок остался в списке")
	}
}

func TestEmptyClipIsAFailure(t *testing.T) {
	r := newRig(&Clip{}, nil)
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	select {
	case err := <-r.failed:
		if !strings.Contains(err.Error(), "нет звука") {
			t.Fatalf("ошибка = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("нет OnFailed")
	}
}

func TestSinkStartFailureReports(t *testing.T) {
	r := newRig(&Clip{PCM: pattern(4000)}, nil)
	r.e.NewSink = func() (Sink, error) { return nil, errors.New("нет aplay") }
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	select {
	case err := <-r.failed:
		if !strings.Contains(err.Error(), "запуск вывода") || !strings.Contains(err.Error(), "нет aplay") {
			t.Fatalf("ошибка = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("нет OnFailed")
	}
}

func TestSinkDyingMidRingReports(t *testing.T) {
	r := newRig(&Clip{PCM: pattern(20000)}, nil)
	r.e.NewSink = func() (Sink, error) {
		s := &fakeSink{failAt: 30000}
		r.mu.Lock()
		r.sinks = append(r.sinks, s)
		r.mu.Unlock()
		return s, nil
	}
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	select {
	case err := <-r.failed:
		if !strings.Contains(err.Error(), "запись в aplay") {
			t.Fatalf("ошибка = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("нет OnFailed")
	}
	if len(r.playing) != 1 {
		t.Fatal("звук шёл, OnPlaying должен был сработать один раз")
	}
	waitFor(t, "aplay остановлен", r.sink(0).aborted.Load)
}

func TestStopWhileStillDecodingStaysSilent(t *testing.T) {
	r := newRig(&Clip{PCM: pattern(4000)}, nil)
	gate := make(chan struct{})
	r.e.Decode = func(string) (*Clip, error) {
		<-gate
		return &Clip{PCM: pattern(4000)}, nil
	}
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	waitFor(t, "aplay поднят параллельно с раскодированием", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.newSinks == 1
	})
	r.e.Stop("a")
	close(gate)
	waitFor(t, "aplay остановлен", r.sink(0).aborted.Load)
	time.Sleep(20 * time.Millisecond)
	if len(r.playing) != 0 || len(r.failed) != 0 {
		t.Fatal("остановленный звонок не должен ни играть, ни жаловаться")
	}
	if len(r.sink(0).bytes()) != 0 {
		t.Fatal("в остановленный aplay что-то записано")
	}
}

func TestSecondStartOfSameFileUsesTheCache(t *testing.T) {
	r := newRig(&Clip{PCM: pattern(4000)}, nil)
	path := "/system/vendor/quasar/sounds/common/timer.wav"
	if err := r.e.Preload(path); err != nil {
		t.Fatal(err)
	}
	r.e.Start("a", path)
	<-r.playing
	r.e.Stop("a")
	r.e.Start("b", path)
	<-r.playing
	r.e.Stop("b")
	if n := r.decodes.Load(); n != 1 {
		t.Fatalf("файл раскодирован %d раз, ждали 1", n)
	}
}

func TestCacheEvictsOldestAndSkipsHugeClips(t *testing.T) {
	e := &Engine{}
	mb := func(n int) *Clip { return &Clip{PCM: make([]byte, n<<20)} }
	e.remember("a", mb(3))
	e.remember("b", mb(2))
	e.remember("c", mb(2)) // 3+2+2 > 6: уходит самый старый
	if e.cache["a"] != nil || e.cache["b"] == nil || e.cache["c"] == nil || e.cacheBytes != 4<<20 {
		t.Fatalf("кэш после вытеснения: a=%v b=%v c=%v байт=%d", e.cache["a"] != nil, e.cache["b"] != nil, e.cache["c"] != nil, e.cacheBytes)
	}
	e.remember("huge", mb(7))
	if e.cache["huge"] != nil {
		t.Fatal("звук больше лимита попал в кэш")
	}
}

func TestShutdownStopsEveryRingAndRefusesNewOnes(t *testing.T) {
	r := newRig(&Clip{PCM: pattern(20000)}, nil)
	r.e.Start("a", "/system/vendor/quasar/sounds/common/timer.wav")
	r.e.Start("b", "/system/vendor/quasar/sounds/common/timer.wav")
	<-r.playing
	<-r.playing
	r.e.Shutdown()
	if !r.sink(0).aborted.Load() || !r.sink(1).aborted.Load() {
		t.Fatal("после Shutdown (он синхронный) не все aplay остановлены")
	}
	r.e.Start("c", "/system/vendor/quasar/sounds/common/timer.wav")
	time.Sleep(20 * time.Millisecond)
	if r.decodes.Load() != 1 || len(r.sinks) != 2 {
		t.Fatal("после Shutdown запущен новый звонок")
	}
}

func TestSameIDRestartReplacesTheOldRing(t *testing.T) {
	r := newRig(&Clip{PCM: pattern(20000)}, nil)
	path := "/system/vendor/quasar/sounds/common/timer.wav"
	r.e.Start("a", path)
	<-r.playing
	r.e.Start("a", path)
	<-r.playing
	waitFor(t, "старый aplay остановлен", r.sink(0).aborted.Load)
	if r.sink(1).aborted.Load() {
		t.Fatal("новый звонок остановлен вместе со старым")
	}
	r.e.Stop("a")
}

func TestValidPath(t *testing.T) {
	ok := []string{
		"/system/vendor/quasar/sounds/common/timer.wav",
		"/system/vendor/quasar/sounds/ru-RU/alarm_1.mp3",
	}
	bad := []string{
		"",
		"/system/vendor/quasar/sounds/",
		"/system/vendor/quasar/sounds/../maind",
		"/system/vendor/quasar/sounds/common/a b.wav",
		"/system/vendor/quasar/sounds/common/a;b.wav",
		"/system/vendor/quasar/sounds/common/a!b.wav",
		"/system/vendor/quasar/sounds/common/$(x).wav",
		"/system/vendor/quasar/soundsX/timer.wav",
		"/tmp/timer.wav",
		"/system/vendor/quasar/sounds/common/\"x\".wav",
		"/system/vendor/quasar/sounds/common/тихо.wav",
	}
	for _, p := range ok {
		if !ValidPath(p) {
			t.Errorf("ValidPath(%q) = false", p)
		}
	}
	for _, p := range bad {
		if ValidPath(p) {
			t.Errorf("ValidPath(%q) = true", p)
		}
	}
}
