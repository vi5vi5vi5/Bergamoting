package music

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openmediad/internal/player"
	"openmediad/internal/spectrum"
)

const fakeFrameBytes = 4096 // PCM на один кадр AAC у поддельного декодера

// fakeDec «декодирует»: на каждый кадр ADTS отдаёт fakeFrameBytes PCM, в первых отсчётах — номер кадра (первый байт полезной нагрузки).
type fakeDec struct {
	pr     *io.PipeReader
	pw     *io.PipeWriter
	closed atomic.Bool
}

func newFakeDec() *fakeDec {
	pr, pw := io.Pipe()
	return &fakeDec{pr: pr, pw: pw}
}

func (d *fakeDec) Write(p []byte) (int, error) {
	pcm := make([]byte, fakeFrameBytes)
	binary.LittleEndian.PutUint16(pcm, uint16(p[7])) // payload[0] — номер кадра (так собран synthMP4)
	if _, err := d.pw.Write(pcm); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (d *fakeDec) Read(p []byte) (int, error) { return d.pr.Read(p) }
func (d *fakeDec) CloseWrite() error          { return d.pw.Close() }
func (d *fakeDec) Close() error {
	d.closed.Store(true)
	d.pw.CloseWithError(io.ErrClosedPipe)
	d.pr.CloseWithError(io.ErrClosedPipe)
	return nil
}

// memSink собирает PCM, который «играл» один aplay.
type memSink struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	aborted  atomic.Bool
	finished atomic.Bool
	delay    time.Duration
}

func (m *memSink) Write(p []byte) (int, error) {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}
func (m *memSink) Abort()  { m.aborted.Store(true) }
func (m *memSink) Finish() { m.finished.Store(true) }
func (m *memSink) bytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.buf.Bytes()...)
}

// frames — номера кадров в PCM (по первому отсчёту каждого блока fakeFrameBytes).
func frames(pcm []byte) (out []int) {
	for i := 0; i+fakeFrameBytes <= len(pcm); i += fakeFrameBytes {
		out = append(out, int(binary.LittleEndian.Uint16(pcm[i:])))
	}
	return
}

type harness struct {
	e      *Engine
	srv    *httptest.Server
	mu     sync.Mutex
	sinks  []*memSink
	decs   []*fakeDec
	opened chan uint64
	failed chan error
	done   chan string
	key    []byte
	n      int
	paced  time.Duration // задержка каждой записи в приёмник: имитирует вывод в реальном времени
}

func newHarness(t *testing.T, n int, status int) *harness {
	h := &harness{opened: make(chan uint64, 4), failed: make(chan error, 4), done: make(chan string, 4), key: []byte("0123456789abcdef"), n: n}
	file, _ := synthMP4(n, func(i int) uint32 { return 120 })
	// payload[0] каждого кадра = i (synthMP4 заполняет кадр байтом i); для i ≥ 256 номер усечётся — тесты используют n < 256
	enc := encrypt(h.key, file)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			http.Error(w, "нет", status)
			return
		}
		http.ServeContent(w, r, "t.mp4", time.Time{}, bytes.NewReader(enc))
	}))
	t.Cleanup(h.srv.Close)
	h.e = &Engine{
		Client: h.srv.Client(),
		NewDec: func() (Decoder, error) {
			d := newFakeDec()
			h.mu.Lock()
			h.decs = append(h.decs, d)
			h.mu.Unlock()
			return d, nil
		},
		NewSink: func() (PCMSink, error) {
			s := &memSink{delay: h.paced}
			h.mu.Lock()
			h.sinks = append(h.sinks, s)
			h.mu.Unlock()
			return s, nil
		},
		NoFadeIn:   true,
		NoRewind:   true, // мгновенный приёмник ничего не «теряет» в буферах: оценка потерь по часам здесь не применима
		Logf:       func(f string, a ...any) { t.Logf(f, a...) },
		OnOpened:   func(id string, d uint64) { h.opened <- d },
		OnFailed:   func(id string, err error) { h.failed <- err },
		OnFinished: func(id string) { h.done <- id },
	}
	t.Cleanup(h.e.Shutdown)
	return h
}

func (h *harness) track(id string) player.Track {
	return player.Track{ID: "t", URL: h.srv.URL, Key: h.key, Title: "тест"}
}

func (h *harness) allPCM() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []byte
	for _, s := range h.sinks {
		out = append(out, s.bytes()...)
	}
	return out
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("не дождались: %s", what)
		panic("unreachable")
	}
}

func TestPlayWholeTrackInOrder(t *testing.T) {
	h := newHarness(t, 40, 0)
	h.e.Open("p1", h.track("p1"), 0)
	dur := waitFor(t, h.opened, "Opened")
	if want := uint64(40 * 1024 * 1000 / 48000); dur != want {
		t.Fatalf("длительность %d мс, ждали %d", dur, want)
	}
	h.e.Start("p1")
	if id := waitFor(t, h.done, "Finished"); id != "p1" {
		t.Fatalf("Finished для %q", id)
	}
	got := frames(h.allPCM())
	if len(got) != 40 {
		t.Fatalf("кадров воспроизведено %d, ждали 40", len(got))
	}
	for i, f := range got {
		if f != i {
			t.Fatalf("кадр %d в позиции %d: порядок нарушен", f, i)
		}
	}
	if !h.sinks[0].finished.Load() || h.sinks[0].aborted.Load() {
		t.Fatal("в конце трека aplay должен доиграть (Finish), а не обрываться")
	}
}

func TestOpenedBeforeStartIsBackpressured(t *testing.T) {
	// Трек длиннее буфера: без Start вывод не идёт, а скачивание/декодер встают на заполненном буфере.
	h := newHarness(t, 200, 0)
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	time.Sleep(300 * time.Millisecond)
	if len(h.allPCM()) != 0 {
		t.Fatal("до Start звук выводиться не должен")
	}
	tr := h.e.get("p1")
	if got := tr.sess.ring.Len(); got < openLevel || got > ringBytes {
		t.Fatalf("в буфере %d байт, ждали от %d до %d", got, openLevel, ringBytes)
	}
}

func TestSeekStartsAtRightFrame(t *testing.T) {
	h := newHarness(t, 120, 0)
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Seek("p1", 1000) // 1000 мс → кадр 46 (кадр = 21,333 мс)
	time.Sleep(100 * time.Millisecond)
	h.e.Start("p1")
	waitFor(t, h.done, "Finished")
	got := frames(h.allPCM())
	if len(got) == 0 || got[0] != 46 {
		t.Fatalf("после перемотки на 1000 мс первым должен играть кадр 46, а играет %v", got[:min(3, len(got))])
	}
	if want := 120 - 46; len(got) != want {
		t.Fatalf("воспроизведено %d кадров, ждали %d", len(got), want)
	}
	if pos := h.e.Pos("p1"); pos < 2400 || pos > 2600 {
		t.Fatalf("после доигрывания позиция %d мс, ждали ≈2560 (длительность трека)", pos)
	}
}

func TestPauseResumeKeepsStreamContinuous(t *testing.T) {
	h := newHarness(t, 150, 0)
	h.paced = 8 * time.Millisecond
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Start("p1")
	time.Sleep(50 * time.Millisecond)
	h.e.Pause("p1")
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	first := h.sinks[0]
	h.mu.Unlock()
	if !first.aborted.Load() {
		t.Fatal("пауза должна обрывать aplay сразу")
	}
	posAtPause := h.e.Pos("p1")
	time.Sleep(150 * time.Millisecond)
	if p := h.e.Pos("p1"); p != posAtPause {
		t.Fatalf("на паузе позиция %d сдвинулась до %d", posAtPause, p)
	}
	h.e.Start("p1")
	waitFor(t, h.done, "Finished")
	got := frames(h.allPCM())
	if len(got) < 100 {
		t.Fatalf("воспроизведено слишком мало кадров: %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("разрыв или повтор после паузы: кадр %d идёт за %d", got[i], got[i-1])
		}
	}
}

func TestAdoptKeepsBufferWhenOffsetMatches(t *testing.T) {
	h := newHarness(t, 150, 0)
	h.paced = 8 * time.Millisecond
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Start("p1")
	time.Sleep(50 * time.Millisecond)
	h.e.Pause("p1")
	time.Sleep(50 * time.Millisecond)
	pos := h.e.Pos("p1")
	h.mu.Lock()
	ndec := len(h.decs)
	h.mu.Unlock()
	h.e.Adopt("p1", "p2", pos)
	if h.e.get("p1") != nil || h.e.get("p2") == nil {
		t.Fatal("источник должен перейти к новому id")
	}
	h.e.Start("p2")
	if id := waitFor(t, h.done, "Finished"); id != "p2" {
		t.Fatalf("Finished для %q, ждали p2", id)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.decs) != ndec {
		t.Fatal("возобновление с тем же смещением не должно перезапускать декодер")
	}
}

func TestOpenFailureReported(t *testing.T) {
	h := newHarness(t, 10, 404)
	h.e.Open("p1", h.track("p1"), 0)
	err := waitFor(t, h.failed, "OnFailed")
	if err == nil {
		t.Fatal("ожидали ошибку открытия")
	}
	select {
	case <-h.opened:
		t.Fatal("Opened не должен приходить для неоткрывшегося потока")
	default:
	}
}

func TestCloseTearsEverythingDown(t *testing.T) {
	h := newHarness(t, 150, 0)
	h.paced = 8 * time.Millisecond
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Start("p1")
	time.Sleep(50 * time.Millisecond)
	h.e.Close("p1")
	time.Sleep(150 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.decs[0].closed.Load() {
		t.Fatal("декодер должен быть закрыт")
	}
	if !h.sinks[0].aborted.Load() {
		t.Fatal("вывод должен быть оборван")
	}
	select {
	case id := <-h.done:
		t.Fatalf("после Close не должно быть Finished (%s)", id)
	default:
	}
}

func TestApplyGainRampsAndClips(t *testing.T) {
	mk := func(vals ...int16) []byte {
		b := make([]byte, 2*len(vals))
		for i, v := range vals {
			binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
		}
		return b
	}
	b := mk(1000, 1000, 1000, 1000)
	if g := applyGain(b, 1, 1); g != 1 || !bytes.Equal(b, mk(1000, 1000, 1000, 1000)) {
		t.Fatal("громкость 1 не должна менять отсчёты")
	}
	b = mk(1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000)
	g := applyGain(b, 1, 0.25)
	if g != 0.75 {
		t.Fatalf("за порцию громкость сдвигается на шаг 0,25: %v", g)
	}
	first := int16(binary.LittleEndian.Uint16(b[0:]))
	last := int16(binary.LittleEndian.Uint16(b[14:]))
	if first != 1000 || last >= first {
		t.Fatalf("плавное снижение: первый %d, последний %d", first, last)
	}
	b = mk(30000, -30000)
	applyGain(b, 2, 2)
	if int16(binary.LittleEndian.Uint16(b[0:])) != 32767 || int16(binary.LittleEndian.Uint16(b[2:])) != -32768 {
		t.Fatal("переполнение должно ограничиваться, а не заворачиваться")
	}
}

func TestFadeInRampsFromSilence(t *testing.T) {
	b := make([]byte, fadeInBytes+400)
	for i := 0; i+1 < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(int16(10000)))
	}
	left := fadeIn(b, fadeInBytes)
	if left != 0 {
		t.Fatalf("после полного окна нарастание должно закончиться, осталось %d", left)
	}
	first := int16(binary.LittleEndian.Uint16(b[0:]))
	mid := int16(binary.LittleEndian.Uint16(b[fadeInBytes/2:]))
	end := int16(binary.LittleEndian.Uint16(b[fadeInBytes+100:]))
	if first > 100 || mid < 4000 || mid > 6000 || end != 10000 {
		t.Fatalf("нарастание: начало %d (ждали ≈0), середина %d (≈5000), после окна %d (10000)", first, mid, end)
	}
	// нарастание, разбитое на порции, даёт тот же результат, что целиком
	c := make([]byte, fadeInBytes)
	for i := 0; i+1 < len(c); i += 2 {
		binary.LittleEndian.PutUint16(c[i:], uint16(int16(10000)))
	}
	rem := fadeInBytes
	for off := 0; off < len(c); off += 9600 {
		rem = fadeIn(c[off:min(off+9600, len(c))], rem)
	}
	if !bytes.Equal(c, b[:fadeInBytes]) {
		t.Fatal("нарастание по порциям отличается от нарастания целиком")
	}
}

// specLog собирает события спектра движка.
type specLog struct {
	mu sync.Mutex
	ev []SpectrumEvent
}

func (s *specLog) add(e SpectrumEvent) {
	e.DB = append([]float32(nil), e.DB...)
	s.mu.Lock()
	s.ev = append(s.ev, e)
	s.mu.Unlock()
}

func (s *specLog) all() []SpectrumEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SpectrumEvent(nil), s.ev...)
}

func TestSpectrumFollowsPlayback(t *testing.T) {
	h := newHarness(t, 60, 0)
	var sl specLog
	h.e.OnSpectrum = sl.add
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Start("p1")
	waitFor(t, h.done, "Finished")
	ev := sl.all()
	if len(ev) < 3 || ev[0].Kind != spectrum.KindStart {
		t.Fatalf("первым должно идти сообщение о старте потока, а пришло %+v", ev[:min(2, len(ev))])
	}
	if ev[len(ev)-1].Kind != spectrum.KindStop {
		t.Fatalf("последним — остановка потока, а пришло %d", ev[len(ev)-1].Kind)
	}
	data := 0
	for _, e := range ev {
		if e.Stream != ev[0].Stream {
			t.Fatal("у одного трека один идентификатор потока")
		}
		if e.Kind == spectrum.KindData {
			data++
			if len(e.DB) != spectrum.Bands {
				t.Fatalf("кадр из %d значений вместо %d", len(e.DB), spectrum.Bands)
			}
		}
	}
	// 60 кадров AAC по 4096 Б PCM = 245760 Б = 61440 отсчётов = 32 интервала по 1920 отсчётов
	if want := 60 * fakeFrameBytes / 4 / spectrum.FrameLen; data != want {
		t.Fatalf("кадров спектра %d, ждали %d", data, want)
	}
}

func TestSpectrumPauseStopsAndResumeContinuesStream(t *testing.T) {
	h := newHarness(t, 150, 0)
	h.paced = 8 * time.Millisecond
	var sl specLog
	h.e.OnSpectrum = sl.add
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Start("p1")
	time.Sleep(80 * time.Millisecond)
	h.e.Pause("p1")
	time.Sleep(100 * time.Millisecond)
	h.e.Start("p1")
	time.Sleep(80 * time.Millisecond)
	h.e.Close("p1")
	time.Sleep(100 * time.Millisecond)
	var kinds []int
	var stream uint64
	for _, e := range sl.all() {
		if e.Kind == spectrum.KindData {
			continue
		}
		kinds = append(kinds, e.Kind)
		if stream == 0 {
			stream = e.Stream
		}
		if e.Stream != stream {
			t.Fatal("возобновление должно продолжать тот же поток")
		}
	}
	want := []int{spectrum.KindStart, spectrum.KindStop, spectrum.KindResume, spectrum.KindStop}
	if len(kinds) != len(want) {
		t.Fatalf("служебные сообщения %v, ждали %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("служебные сообщения %v, ждали %v", kinds, want)
		}
	}
}

func TestDecoderStartFailureReported(t *testing.T) {
	h := newHarness(t, 20, 0)
	h.e.NewDec = func() (Decoder, error) { return nil, io.ErrUnexpectedEOF }
	h.e.Open("p1", h.track("p1"), 0)
	if err := waitFor(t, h.failed, "OnFailed"); err == nil {
		t.Fatal("ошибка запуска декодера должна сообщаться (откат на mediad)")
	}
}

func TestReopenOfOpenedTrackRenotifies(t *testing.T) {
	h := newHarness(t, 30, 0)
	h.e.Open("p1", h.track("p1"), 0)
	d1 := waitFor(t, h.opened, "первое Opened")
	h.e.Open("p1", h.track("p1"), 0) // повторный prefetch
	d2 := waitFor(t, h.opened, "повторное Opened")
	if d1 != d2 {
		t.Fatalf("длительность в повторном уведомлении %d, в первом %d", d2, d1)
	}
	h.mu.Lock()
	n := len(h.decs)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("повторное открытие не должно запускать второй декодер, запущено %d", n)
	}
}

// applyGain в целых числах не должна расходиться с расчётом в плавающей точке больше чем на единицу младшего разряда.
func TestApplyGainMatchesFloatReference(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, c := range []struct{ cur, target float64 }{{0.4, 0.4}, {1, 0.25}, {0.25, 1}, {0.7, 0.72}, {1, 2}, {0.43, 0.43}} {
		b := make([]byte, 2*5000)
		ref := make([]float64, 5000)
		for i := range ref {
			v := int16(r.Intn(65536) - 32768)
			binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
			ref[i] = float64(v)
		}
		next := applyGain(b, c.cur, c.target)
		step := next
		if d := c.target - c.cur; math.Abs(d) > ductRampPerCh {
			step = c.cur + math.Copysign(ductRampPerCh, d)
		}
		if next != step {
			t.Fatalf("громкость к концу порции %v, ждали %v", next, step)
		}
		for i := range ref {
			g := c.cur + (next-c.cur)*float64(i)/float64(len(ref))
			want := math.Max(-32768, math.Min(32767, math.Round(ref[i]*g)))
			got := float64(int16(binary.LittleEndian.Uint16(b[2*i:])))
			if math.Abs(got-want) > 1 {
				t.Fatalf("громкость %v→%v, отсчёт %d: %v, ждали %v", c.cur, c.target, i, got, want)
			}
		}
	}
}

// Пауза выбрасывает то, что лежало в трубе и буферах aplay; шим возвращает это в буфер, и после «продолжи» звук идёт с места,
// где реально замолчал динамик. Часы подменены: «прошло» 400 мс при «записано» около 240 КБ (1,2 с звука).
func TestPauseRewindsAudioThatNeverReachedSpeaker(t *testing.T) {
	h := newHarness(t, 150, 0)
	h.paced = 8 * time.Millisecond // вывод в 6 раз быстрее реального времени: записано больше, чем прозвучало бы
	h.e.NoRewind = false
	var clk atomic.Int64 // прошедшее время цикла вывода, нс
	base := time.Now()
	h.e.Now = func() time.Time { return base.Add(time.Duration(clk.Load())) }
	h.e.Open("p1", h.track("p1"), 0)
	waitFor(t, h.opened, "Opened")
	h.e.Start("p1")
	time.Sleep(200 * time.Millisecond)
	clk.Store(int64(400 * time.Millisecond)) // прозвучало 400 − 40 (задержка) = 360 мс = 69120 байт
	h.e.Pause("p1")
	time.Sleep(150 * time.Millisecond)
	written := len(h.sinks[0].bytes())
	const audible = 360 * pcmBytesPerMs
	if written < audible+9600 {
		t.Fatalf("приёмник принял лишь %d байт: для проверки нужно заметно больше %d", written, audible)
	}
	if p := h.e.Pos("p1"); p < 355 || p > 365 {
		t.Fatalf("позиция после паузы %d мс, ждали ≈360 (то, что прозвучало), а не %d (то, что записано)", p, written/pcmBytesPerMs)
	}
	h.e.Start("p1")
	waitFor(t, h.done, "Finished")
	second := h.sinks[1].bytes()
	if len(second) < 4096 {
		t.Fatalf("после «продолжи» второй приёмник получил лишь %d байт", len(second))
	}
	if !bytes.Equal(second[:4096], expectedStream(150)[audible:audible+4096]) {
		t.Fatalf("звук после паузы должен продолжаться с байта %d (то, что прозвучало), а продолжился с другого места", audible)
	}
}

// expectedStream — весь PCM трека у поддельного декодера: кадр i даёт fakeFrameBytes байт, первые два — номер кадра.
func expectedStream(frames int) []byte {
	out := make([]byte, frames*fakeFrameBytes)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(out[i*fakeFrameBytes:], uint16(i))
	}
	return out
}
