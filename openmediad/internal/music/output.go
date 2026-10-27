package music

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"time"
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
	t.mu.Unlock()
	go e.outLoop(t, gen)
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
	pos := base + t.played.Load()/pcmBytesPerMs
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
func (e *Engine) outLoop(t *track, gen int) {
	sk, err := e.NewSink()
	if err != nil {
		e.logf("вывод: %v", err)
		return
	}
	done := false // aplay уже доиграл и закрыт (иначе при выходе его обрываем)
	defer func() {
		if !done {
			sk.Abort()
		}
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
				done = true
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
		if _, err := sk.Write(buf[:n]); err != nil {
			e.logf("вывод: запись в aplay: %v", err)
			return
		}
		t.played.Add(uint64(n))
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

// applyGain умножает отсчёты S16LE на громкость, плавно меняя её от cur к target в пределах порции.
// Возвращает громкость к концу порции.
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
	for i := 0; i < n; i++ {
		g := cur + (next-cur)*float64(i)/float64(n)
		v := float64(int16(binary.LittleEndian.Uint16(b[2*i:]))) * g
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(math.Max(-32768, math.Min(32767, math.Round(v))))))
	}
	return next
}

func sleepShort() { time.Sleep(10 * time.Millisecond) }

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
