package music

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"time"

	"openmediad/internal/clock"
	"openmediad/internal/spectrum"
)

// Start начинает (или продолжает) вывод трека. Для ещё не открытого трека вывод сам дождётся звука.
func (e *Engine) Start(id string) {
	t := e.get(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed || t.playing {
		t.mu.Unlock()
		return
	}
	t.playing = true
	t.outGen++
	gen := t.outGen
	prev := t.loopDone
	done := make(chan struct{})
	t.loopDone = done
	t.mu.Unlock()
	go e.outLoop(t, gen, prev, done)
}

// audioLatency — через сколько после записи в aplay звук слышен: старт PCM и буфер смесителя. По замерам до запуска PCM
// ядром проходит 14–30 мс; оценка нужна только для возврата звука при паузе и для позиции.
const audioLatency = 40 * time.Millisecond

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Pause останавливает вывод; источник, буфер и позиция остаются (как кэш плееров mediad).
func (e *Engine) Pause(id string) {
	t := e.get(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	t.playing = false
	t.outGen++
	s := t.sess
	t.mu.Unlock()
	if s != nil {
		s.ring.Wake()
	}
}

// Close освобождает трек целиком: скачивание, декодер, буфер, вывод.
func (e *Engine) Close(id string) {
	e.mu.Lock()
	e.init()
	t := e.tracks[id]
	delete(e.tracks, id)
	e.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	t.closed = true
	t.playing = false
	t.outGen++
	s := t.sess
	t.sess = nil
	t.mu.Unlock()
	t.cancel()
	if s != nil {
		t.drop(s)
	}
}

// drop останавливает сессию: буфер, запросы, декодер (процесс гасится в фоне, чтобы не задерживать вызывающего).
func (t *track) drop(s *session) {
	s.cancel()
	s.ring.Stop()
	go s.dec.Close()
}

// Adopt передаёт открытый источник другому плееру (возобновление с паузы: platformd шлёт play с новым id).
func (e *Engine) Adopt(from, to string, offsetMs uint64) {
	e.mu.Lock()
	e.init()
	t := e.tracks[from]
	if t == nil || e.tracks[to] != nil {
		e.mu.Unlock()
		return
	}
	delete(e.tracks, from)
	t.id = to
	e.tracks[to] = t
	e.mu.Unlock()
	// Если смещение отличается от места паузы заметно, надо перейти (иначе продолжаем с буфера без перезапуска).
	cur := e.Pos(to)
	if d := int64(offsetMs) - int64(cur); d > 1500 || d < -1500 {
		e.Seek(to, offsetMs)
	}
}

// Seek переходит к позиции: старая сессия бросается, новая начинается с нужного кадра (Range-запрос).
func (e *Engine) Seek(id string, posMs uint64) {
	t := e.get(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed || t.idx == nil {
		t.mu.Unlock()
		return
	}
	old := t.sess
	t.sess = nil
	t.mu.Unlock()
	if old != nil {
		t.drop(old)
	}
	go t.start(nil, posMs)
}

// Duck задаёт громкость музыки (1 — как есть; меньше — приглушение на время диалога).
func (e *Engine) Duck(g float64) {
	e.mu.Lock()
	e.init()
	e.mu.Unlock()
	e.gainBits.Store(math.Float64bits(g))
}

// Pos — позиция воспроизведения, мс (по числу байт, переданных выводу).
func (e *Engine) Pos(id string) uint64 {
	t := e.get(id)
	if t == nil {
		return 0
	}
	t.mu.Lock()
	base, idx := t.baseMs, t.idx
	t.mu.Unlock()
	played := t.played.Load()
	if at := t.outAt.Load(); at != 0 { // идёт вывод: позиция не может опережать часы (байты в трубе ещё не прозвучали)
		el := e.now().Sub(time.Unix(0, at)) - audioLatency
		if el < 0 {
			el = 0
		}
		if byClock := t.outPlayed.Load() + uint64(el/time.Millisecond)*pcmBytesPerMs; byClock < played {
			played = byClock
		}
	}
	pos := base + played/pcmBytesPerMs
	if idx != nil && pos > idx.DurationMs {
		pos = idx.DurationMs
	}
	return pos
}

// Net — сколько байт скачано и какова длина потока (идёт в diag события).
func (e *Engine) Net(id string) (uint64, uint64) {
	t := e.get(id)
	if t == nil {
		return 0, 0
	}
	return t.got.Load(), uint64(max(t.tot.Load(), 0))
}

// Shutdown останавливает всё (выход шима).
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.init()
	ids := make([]string, 0, len(e.tracks))
	for id := range e.tracks {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.Close(id)
	}
}

// outLoop гонит PCM трека из буфера в aplay, пока не сменится поколение вывода. В конце трека доигрывает остаток
// и сообщает Finished; при остановке (пауза, закрытие) обрывает aplay сразу.
func (e *Engine) outLoop(t *track, gen int, prev, done chan struct{}) {
	defer close(done)
	if prev != nil {
		<-prev // прежний цикл должен успеть вернуть непрозвучавшее в буфер, прежде чем начнёт читать этот
	}
	sk, err := e.NewSink()
	if err != nil {
		e.logf("вывод: %v", err)
		return
	}
	finished := false // aplay уже доиграл и закрыт (иначе при выходе его обрываем)
	an := e.startSpectrum(t)
	startAt := e.now()
	startPlayed := t.played.Load()
	t.mu.Lock()
	startSess := t.sess
	t.mu.Unlock()
	t.outPlayed.Store(startPlayed)
	defer func() {
		if !finished {
			sk.Abort()
			e.rewindLost(t, startSess, gen, startAt, startPlayed)
		}
		t.outAt.Store(0)
		e.stopSpectrum(t, an)
	}()
	buf := make([]byte, chunkBytes)
	norm := math.Pow(10, t.src.GainDB/20) // нормализация трека к −14 LUFS
	cur := math.Float64frombits(e.gainBits.Load()) * norm
	fade := fadeInBytes // плавное нарастание в начале вывода: без щелчка при старте и возобновлении
	if e.NoFadeIn {
		fade = 0
	}
	for {
		t.mu.Lock()
		stop := t.outGen != gen || t.closed
		s := t.sess
		t.mu.Unlock()
		if stop {
			return
		}
		if s == nil { // идёт перемотка: сессия ещё не создана
			sleepShort()
			continue
		}
		n, err := s.ring.ReadWait(buf)
		switch {
		case errors.Is(err, errWake), errors.Is(err, errStopped):
			continue
		case errors.Is(err, io.EOF):
			t.mu.Lock()
			same := t.sess == s && t.outGen == gen && !t.closed
			t.mu.Unlock()
			if same {
				finished = true
				e.finish(t, sk)
			}
			return
		case err != nil:
			e.logf("вывод: чтение буфера: %v", err)
			return
		}
		if fade > 0 {
			fade = fadeIn(buf[:n], fade)
		}
		target := math.Float64frombits(e.gainBits.Load()) * norm
		cur = applyGain(buf[:n], cur, target)
		if an != nil {
			an.Write(buf[:n]) // спектр считается по тому же звуку, что уходит на динамик (после нормализации и приглушения)
		}
		if _, err := sk.Write(buf[:n]); err != nil {
			e.logf("вывод: запись в aplay: %v", err)
			return
		}
		t.played.Add(uint64(n))
		t.outAt.CompareAndSwap(0, startAt.UnixNano()) // «играет» с момента первой записи цикла
	}
}

// rewindLost вызывается при выходе из цикла вывода. Если цикл остановила пауза (та же сессия, трек жив), то часть
// записанного в aplay до динамика не дошла: труба и буферы aplay и смесителя держат до ≈0,5 с, а Abort их выбрасывает. Этот
// звук возвращается в буфер, и после «продолжи» трек идёт с того места, где реально замолчал, без провала.
func (e *Engine) rewindLost(t *track, startSess *session, gen int, startAt time.Time, startPlayed uint64) {
	if e.NoRewind {
		return
	}
	t.mu.Lock()
	// outGen изменился: была пауза (возможно, уже и новый Start поверх неё: он ждёт завершения этого цикла)
	pause := !t.closed && t.sess == startSess && t.outGen != gen && startSess != nil
	t.mu.Unlock()
	if !pause {
		return
	}
	written := t.played.Load() - startPlayed
	audible := uint64(0)
	if el := e.now().Sub(startAt) - audioLatency; el > 0 {
		audible = uint64(el/time.Millisecond) * pcmBytesPerMs
	}
	if written <= audible {
		return
	}
	lost := int((written - audible) / 4 * 4) // целое число стереокадров
	if k := startSess.ring.Rewind(lost); k > 0 {
		t.played.Add(^uint64(k) + 1) // played -= k
		e.logf("пауза: возвращено в буфер %d мс звука, не дошедшего до динамика", k/pcmBytesPerMs)
	}
}

// finish доигрывает остаток в aplay и сообщает о конце трека (позиция в этот момент равна длительности).
func (e *Engine) finish(t *track, sk PCMSink) {
	sk.Finish()
	t.mu.Lock()
	t.playing = false
	t.mu.Unlock()
	if e.OnFinished != nil {
		e.OnFinished(t.id)
	}
}

// applyGain умножает отсчёты S16LE на громкость, плавно меняя её от cur к target в пределах порции (не больше
// ductRampPerCh за порцию). Возвращает громкость к концу порции. Арифметика целочисленная (Q16 и Q32): на колонке
// вариант с math.Max/Round на каждый отсчёт съедал около 3 % ядра.
func applyGain(b []byte, cur, target float64) float64 {
	if cur == 1 && target == 1 {
		return 1
	}
	n := len(b) / 2
	if n == 0 {
		return cur
	}
	next := target
	if d := target - cur; math.Abs(d) > ductRampPerCh {
		next = cur + math.Copysign(ductRampPerCh, d)
	}
	if next == cur { // постоянная громкость: обычный случай
		g := int32(math.Round(cur * 65536))
		for i := 0; i < n; i++ {
			put16(b[2*i:], scale16(int32(int16(binary.LittleEndian.Uint16(b[2*i:]))), g))
		}
		return next
	}
	g := int64(cur * (1 << 32))                          // громкость Q32 в начале порции
	step := int64((next - cur) * (1 << 32) / float64(n)) // шаг на отсчёт
	for i := 0; i < n; i++ {
		put16(b[2*i:], scale16(int32(int16(binary.LittleEndian.Uint16(b[2*i:]))), int32(g>>16)))
		g += step
	}
	return next
}

// scale16 умножает отсчёт на громкость в Q16 с округлением и ограничением по диапазону int16.
func scale16(v, g int32) int16 {
	x := (int64(v)*int64(g) + 32768) >> 16
	switch {
	case x > 32767:
		return 32767
	case x < -32768:
		return -32768
	}
	return int16(x)
}

func put16(b []byte, v int16) { binary.LittleEndian.PutUint16(b, uint16(v)) }

// fadeIn плавно поднимает громкость от нуля на первых left байтах PCM; возвращает, сколько ещё осталось.
func fadeIn(b []byte, left int) int {
	n := len(b) / 2
	total := fadeInBytes / 2
	rem := left / 2
	for i := 0; i < n && rem > 0; i++ {
		g := 1 - float64(rem)/float64(total)
		v := float64(int16(binary.LittleEndian.Uint16(b[2*i:]))) * g
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(math.Round(v))))
		rem--
	}
	return rem * 2
}

// TrackInfo — снимок трека движка для отладочной страницы.
type TrackInfo struct {
	ID      string  `json:"id"`
	Title   string  `json:"title,omitempty"`
	Playing bool    `json:"playing"`
	Opened  bool    `json:"opened"`
	PosMs   uint64  `json:"pos_ms"`
	GotKB   uint64  `json:"downloaded_kb"`
	TotalKB uint64  `json:"total_kb"`
	RingMs  int     `json:"buffered_ms"`
	NormDB  float64 `json:"norm_db"`
}

// Snapshot возвращает открытые треки: сколько скачано, сколько звука в буфере, играет ли.
func (e *Engine) Snapshot() []TrackInfo {
	e.mu.Lock()
	e.init()
	ts := make([]*track, 0, len(e.tracks))
	for _, t := range e.tracks {
		ts = append(ts, t)
	}
	e.mu.Unlock()
	out := make([]TrackInfo, 0, len(ts))
	for _, t := range ts {
		t.mu.Lock()
		ti := TrackInfo{ID: t.id[:min(8, len(t.id))], Title: t.src.Title, Playing: t.playing, Opened: t.opened, NormDB: t.src.GainDB}
		s := t.sess
		t.mu.Unlock()
		ti.PosMs = e.Pos(t.id)
		ti.GotKB, ti.TotalKB = t.got.Load()>>10, uint64(max(t.tot.Load(), 0))>>10
		if s != nil {
			ti.RingMs = s.ring.Len() / pcmBytesPerMs
		}
		out = append(out, ti)
	}
	return out
}

// SpectrumEvent — событие потока спектра (общий тип для музыки и голоса).
type SpectrumEvent = spectrum.Event

// startSpectrum открывает поток спектра трека: первый вывод — Start, возобновление после паузы — Resume с тем же потоком.
func (e *Engine) startSpectrum(t *track) *spectrum.Analyzer {
	if e.OnSpectrum == nil {
		return nil
	}
	t.mu.Lock()
	id := t.spec
	kind := spectrum.KindResume
	if id == 0 {
		id = uint64(clock.Mono())
		t.spec = id
		kind = spectrum.KindStart
	}
	t.mu.Unlock()
	e.OnSpectrum(SpectrumEvent{Kind: kind, Source: spectrum.SourceMusic, Stream: id, At: time.Now()})
	return spectrum.New(func(db []float32) {
		e.OnSpectrum(SpectrumEvent{Kind: spectrum.KindData, Source: spectrum.SourceMusic, Stream: id, DB: db, At: time.Now()})
	})
}

// stopSpectrum сообщает об остановке потока (пауза, конец, закрытие).
func (e *Engine) stopSpectrum(t *track, an *spectrum.Analyzer) {
	if an == nil {
		return
	}
	t.mu.Lock()
	id := t.spec
	t.mu.Unlock()
	e.OnSpectrum(SpectrumEvent{Kind: spectrum.KindStop, Source: spectrum.SourceMusic, Stream: id, At: time.Now()})
}

func sleepShort() { time.Sleep(10 * time.Millisecond) }
