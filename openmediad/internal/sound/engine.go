// Package sound — воспроизведение системных звуков колонки своими силами: звонок таймера и будильника, сигналы. Файл
// раскодируется целиком в память (GStreamer колонки, как у музыки), после чего PCM крутится по кругу через aplay, пока звонок не
// остановят. События плеера выпускает player.Machine; этот пакет только играет и сообщает: «первый звук ушёл» (OnPlaying) и
// «не получилось» (OnFailed). Нужен, чтобы шим забрал у mediad и этот вид звука (фаза 5).
package sound

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"openmediad/internal/sink"
)

const (
	// bytesPerMs — стерео S16LE 48 кГц: 192 байта на миллисекунду.
	bytesPerMs = 192
	// chunkBytes — сколько PCM за одну запись в aplay (≈43 мс): между записями проверяется остановка.
	chunkBytes = 8192
	// cacheLimit — сколько раскодированного звука держим для повторных звонков (≈30 с стерео).
	cacheLimit = 6 << 20
)

// Clip — раскодированный звук.
type Clip struct {
	PCM     []byte // S16LE 48 кГц стерео, длина кратна 4
	Bitrate uint64 // кбит/с у MP3; 0 — не MP3 или неизвестно
}

// DurMs — длительность одного прохода, мс.
func (c *Clip) DurMs() uint64 { return uint64(len(c.PCM)) / bytesPerMs }

// Sink — приёмник звука: в бою aplay, в тестах память.
type Sink interface {
	Write(p []byte) (int, error)
	Abort() // оборвать сразу (SIGINT, не SIGKILL)
}

type aplaySink struct{ p *sink.Player }

func (a aplaySink) Write(b []byte) (int, error) { return a.p.Write(b) }
func (a aplaySink) Abort()                      { a.p.Abort(2 * time.Second) }

// NewAplaySink запускает aplay (path пустой — по умолчанию): стерео 48 кГц.
func NewAplaySink(path string) (Sink, error) {
	p, err := sink.Start(sink.Config{Path: path, Channels: 2})
	if err != nil {
		return nil, err
	}
	return aplaySink{p}, nil
}

// Engine играет любое число звонков одновременно (таймер и будильник могут звучать вместе): у каждого свой aplay,
// смешивает их общий смеситель колонки.
type Engine struct {
	NewSink   func() (Sink, error)
	Decode    func(path string) (*Clip, error) // nil — DecodeFile (Linux: GStreamer колонки)
	Logf      func(string, ...any)
	AplayPath string // путь к aplay для NewSink по умолчанию (пусто — /usr/bin/aplay); нужен проверке отката

	OnPlaying func(id string, durMs, bitrate uint64)
	OnFailed  func(id string, err error)

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool

	cache      map[string]*Clip
	cacheOrder []string
	cacheBytes int
}

func (e *Engine) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

type session struct {
	e       *Engine
	id      string
	path    string
	written atomic.Uint64 // байт PCM, переданных выводу (по всем проходам)
	startAt atomic.Int64  // unix-наносекунды первой записи в aplay (0 — звука ещё не было)
	durMs   atomic.Uint64 // длительность прохода, мс (0 — файл ещё не раскодирован)
	aborted atomic.Bool

	mu sync.Mutex // защищает sk
	sk Sink
}

func (s *session) setSink(k Sink) {
	s.mu.Lock()
	s.sk = k
	s.mu.Unlock()
}

// abort обрывает вывод, если он уже запущен; запуск после abort сам заметит флаг.
func (s *session) abort() {
	s.aborted.Store(true)
	s.mu.Lock()
	k := s.sk
	s.mu.Unlock()
	if k != nil {
		k.Abort()
	}
}

// elapsedMs — сколько звонок звучит. Записанные в aplay байты опережают динамик на размер буферов, поэтому берём меньшее из
// «записано» и «прошло времени с первой записи» (так же считается позиция голоса и музыки).
func (s *session) elapsedMs() uint64 {
	at := s.startAt.Load()
	if at == 0 {
		return 0
	}
	el := uint64(time.Since(time.Unix(0, at)) / time.Millisecond)
	return min(el, s.written.Load()/bytesPerMs)
}

// Start начинает звонок: раскодирует файл и играет по кругу, пока не придёт Stop. Путь должен быть из ValidPath.
func (e *Engine) Start(id, path string) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if e.sessions == nil {
		e.sessions = map[string]*session{}
	}
	s := &session{e: e, id: id, path: path}
	old := e.sessions[id]
	e.sessions[id] = s
	e.mu.Unlock()
	if old != nil {
		old.abort()
	}
	go s.run()
}

// Stop обрывает звонок сразу. Остановка aplay идёт в фоне: вызывающий (разбор команды шима) не ждёт его завершения.
func (e *Engine) Stop(id string) {
	e.mu.Lock()
	s := e.sessions[id]
	delete(e.sessions, id)
	e.mu.Unlock()
	if s != nil {
		s.aborted.Store(true)
		go s.abort()
	}
}

func (e *Engine) get(id string) *session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[id]
}

// Pos — позиция внутри текущего прохода файла, мс (после конца файла отсчёт идёт заново).
func (e *Engine) Pos(id string) uint64 {
	s := e.get(id)
	if s == nil {
		return 0
	}
	d := s.durMs.Load()
	if d == 0 {
		return 0
	}
	return s.elapsedMs() % d
}

// Elapsed — сколько всего звонок звучит, мс.
func (e *Engine) Elapsed(id string) uint64 {
	if s := e.get(id); s != nil {
		return s.elapsedMs()
	}
	return 0
}

// Shutdown обрывает все звонки и больше не принимает новые. Возвращается, когда все aplay остановлены (SIGKILL не бывает:
// ждём SIGINT).
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.closed = true
	all := e.sessions
	e.sessions = nil
	e.mu.Unlock()
	for _, s := range all {
		s.abort()
	}
}

// Preload раскодирует файл заранее и кладёт в кэш: первый звонок тогда стартует без запуска GStreamer.
func (e *Engine) Preload(path string) error {
	_, err := e.clip(path)
	return err
}

func (e *Engine) clip(path string) (*Clip, error) {
	e.mu.Lock()
	c := e.cache[path]
	e.mu.Unlock()
	if c != nil {
		return c, nil
	}
	dec := e.Decode
	if dec == nil {
		dec = DecodeFile
	}
	c, err := dec(path)
	if err != nil {
		return nil, err
	}
	if len(c.PCM) == 0 {
		return nil, fmt.Errorf("в %s нет звука", path)
	}
	e.mu.Lock()
	e.remember(path, c)
	e.mu.Unlock()
	return c, nil
}

// remember кладёт звук в кэш, вытесняя самые старые, пока не уложимся в cacheLimit. Звук больше лимита в кэш не идёт.
func (e *Engine) remember(path string, c *Clip) {
	if len(c.PCM) > cacheLimit || e.cache[path] != nil {
		return
	}
	if e.cache == nil {
		e.cache = map[string]*Clip{}
	}
	for e.cacheBytes+len(c.PCM) > cacheLimit && len(e.cacheOrder) > 0 {
		old := e.cacheOrder[0]
		e.cacheOrder = e.cacheOrder[1:]
		e.cacheBytes -= len(e.cache[old].PCM)
		delete(e.cache, old)
	}
	e.cache[path] = c
	e.cacheOrder = append(e.cacheOrder, path)
	e.cacheBytes += len(c.PCM)
}

func (e *Engine) fail(s *session, err error) {
	e.mu.Lock()
	if e.sessions[s.id] == s {
		delete(e.sessions, s.id)
	}
	e.mu.Unlock()
	if s.aborted.Load() {
		return
	}
	if e.OnFailed != nil {
		e.OnFailed(s.id, err)
	}
}

type sinkResult struct {
	sk  Sink
	err error
}

// run — рабочая горутина звонка: aplay поднимается параллельно с раскодированием, потом PCM идёт по кругу.
func (s *session) run() {
	e := s.e
	newSink := e.NewSink
	if newSink == nil {
		newSink = func() (Sink, error) { return NewAplaySink(e.AplayPath) }
	}
	skc := make(chan sinkResult, 1)
	go func() {
		sk, err := newSink()
		skc <- sinkResult{sk, err}
	}()
	clip, derr := e.clip(s.path)
	r := <-skc
	if derr != nil || r.err != nil || s.aborted.Load() {
		if r.sk != nil {
			r.sk.Abort()
		}
		switch {
		case s.aborted.Load():
		case derr != nil:
			e.fail(s, fmt.Errorf("раскодирование %s: %w", s.path, derr))
		default:
			e.fail(s, fmt.Errorf("запуск вывода: %w", r.err))
		}
		return
	}
	sk := r.sk
	s.durMs.Store(clip.DurMs())
	s.setSink(sk)
	if s.aborted.Load() { // остановили, пока aplay запускался
		sk.Abort()
		return
	}
	pcm := clip.PCM
	playing := false
	for off := 0; ; {
		if s.aborted.Load() {
			return
		}
		end := min(off+chunkBytes, len(pcm))
		if _, err := sk.Write(pcm[off:end]); err != nil {
			if s.aborted.Load() {
				return
			}
			sk.Abort()
			e.fail(s, fmt.Errorf("запись в aplay: %w", err))
			return
		}
		s.written.Add(uint64(end - off))
		if !playing {
			playing = true
			s.startAt.Store(time.Now().UnixNano())
			if e.OnPlaying != nil {
				e.OnPlaying(s.id, clip.DurMs(), clip.Bitrate)
			}
		}
		if off = end; off == len(pcm) {
			off = 0
		}
	}
}
