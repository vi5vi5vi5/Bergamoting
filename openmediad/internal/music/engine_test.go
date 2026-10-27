package music

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openmediad/internal/player"
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
