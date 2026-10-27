// Package voice — воспроизведение голоса Алисы своими силами: чанки Ogg/Opus из команд voice-chunk → декодер Opus (чистый Go) →
// aplay на общий смеситель колонки; параллельно считается спектр для дисплея. Нужен, чтобы шим мог забрать голос у mediad
// (фаза 5) и направлять его куда угодно, например в Bluetooth-наушники. События плеера выпускает player.Machine, а этот
// пакет только играет и сообщает: «первый звук ушёл» (OnPlaying), «доиграно» (OnEnded), «не получилось» (OnFailed).
package voice

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"openmediad/internal/clock"
	"openmediad/internal/opusstream"
	"openmediad/internal/sink"
	"openmediad/internal/spectrum"
)

// bytesPerMs — моно S16LE 48 кГц: 96 байт на миллисекунду.
const bytesPerMs = 96

// Sink — приёмник звука: в бою aplay, в тестах память.
type Sink interface {
	Write(p []byte) (int, error)
	Abort()  // оборвать сразу
	Finish() // доиграть остаток и закрыть
}

type aplaySink struct{ p *sink.Player }

func (a aplaySink) Write(b []byte) (int, error) { return a.p.Write(b) }
func (a aplaySink) Abort()                      { a.p.Abort(2 * time.Second) }
func (a aplaySink) Finish()                     { a.p.Finish(5 * time.Second) }

// NewAplaySink запускает aplay (path пустой — по умолчанию): моно 48 кГц, стерео из него делает plug ALSA
// (так же играл голос в проверке на колонке).
func NewAplaySink(path string) (Sink, error) {
	p, err := sink.Start(sink.Config{Path: path, Channels: 1})
	if err != nil {
		return nil, err
	}
	return aplaySink{p}, nil
}

// Decoder — декодер Ogg/Opus: чанки на входе, PCM S16LE моно 48 кГц на выходе.
type Decoder interface {
	Reset()
	Write(chunk []byte, sink func(pcm []byte) error) error
}

// NewOpusDecoder создаёт настоящий декодер. tap получает выброшенные pre-skip отсчёты (их штатный путь отдаёт спектру).
func NewOpusDecoder(tap func(pcm []byte)) (Decoder, error) {
	s, err := opusstream.New()
	if err != nil {
		return nil, err
	}
	s.PreSkipTap = tap
	return s, nil
}

// Engine играет один ответ Алисы за раз. Новый Start вытесняет прежний ответ.
type Engine struct {
	NewSink    func() (Sink, error)
	NewDecoder func(tap func(pcm []byte)) (Decoder, error)
	Logf       func(string, ...any)
	AplayPath  string // путь к aplay для NewSink по умолчанию (пусто — /usr/bin/aplay); нужен проверке отката

	OnPlaying  func(id string)
	OnEnded    func(id string)
	OnFailed   func(id string, err error)
	OnSpectrum func(spectrum.Event)

	mu     sync.Mutex
	cur    *session
	closed bool

	// Декодер один на весь срок жизни: новый на каждый ответ раздувал бы память (≈3 МБ на декодер Opus). Им пользуется
	// один ответ за раз: рабочая горутина держит decMu от начала до конца ответа.
	decMu  sync.Mutex
	dec    Decoder
	curTap func(pcm []byte) // получатель pre-skip отсчётов текущего ответа
}

func (e *Engine) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

type opKind uint8

const (
	opChunk opKind = iota
	opEOS
)

type op struct {
	kind opKind
	data []byte
}

type session struct {
	e       *Engine
	id      string
	ops     chan op
	played  atomic.Uint64 // байт PCM, переданных выводу
	startAt atomic.Int64  // unix-наносекунды первой записи в aplay (0 — звука ещё не было)
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

// Start готовит вывод ответа: по voice-start поднимается aplay, чтобы первый звук не ждал его запуска (≈14 мс).
func (e *Engine) Start(id string) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	old := e.cur
	s := &session{e: e, id: id, ops: make(chan op, 256)}
	e.cur = s
	e.mu.Unlock()
	if old != nil {
		old.abort()
	}
	go s.run()
}

// Chunk передаёт кусок Ogg/Opus очередного voice-chunk.
func (e *Engine) Chunk(id string, ogg []byte) { e.enqueue(id, op{opChunk, ogg}) }

// EOS сообщает, что чанков больше не будет (voice-eos): после доигрывания придёт OnEnded.
func (e *Engine) EOS(id string) { e.enqueue(id, op{kind: opEOS}) }

func (e *Engine) enqueue(id string, o op) {
	e.mu.Lock()
	s := e.cur
	e.mu.Unlock()
	if s == nil || s.id != id {
		return
	}
	select {
	case s.ops <- o:
	default:
		e.logf("голос %s: очередь чанков переполнена, кусок отброшен", id)
	}
}

// Abort обрывает ответ немедленно (перебили, команда release раньше конца, остановка шима).
func (e *Engine) Abort(id string) {
	e.mu.Lock()
	s := e.cur
	if s != nil && s.id == id {
		e.cur = nil
	} else {
		s = nil
	}
	e.mu.Unlock()
	if s != nil {
		s.abort()
		select { // разбудить рабочую горутину, если она ждёт чанка
		case s.ops <- op{kind: opEOS}:
		default:
		}
	}
}

// Pos — позиция воспроизведения, мс. Записанные в aplay байты опережают динамик на размер буферов (труба 64 КБ — 0,7 с звука,
// плюс буфер смесителя), поэтому позиция — меньшее из «записано» и «прошло времени с первой записи»: так progress идёт
// ровно по часам, как у mediad (1054, 2128 мс…), а не вперёд.
func (e *Engine) Pos(id string) uint64 {
	e.mu.Lock()
	s := e.cur
	e.mu.Unlock()
	if s == nil || s.id != id {
		return 0
	}
	written := s.played.Load() / bytesPerMs
	if at := s.startAt.Load(); at != 0 {
		if el := uint64(time.Since(time.Unix(0, at)) / time.Millisecond); el < written {
			return el
		}
	}
	return written
}

// Shutdown обрывает текущий ответ и больше не принимает новые.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.closed = true
	s := e.cur
	e.cur = nil
	e.mu.Unlock()
	if s != nil {
		s.abort()
	}
}

func (e *Engine) fail(s *session, err error) {
	if s.aborted.Load() {
		return
	}
	if e.OnFailed != nil {
		e.OnFailed(s.id, err)
	}
}

// run — рабочая горутина ответа: по порядку декодирует чанки и пишет PCM в aplay.
func (s *session) run() {
	e := s.e
	if e.NewSink == nil {
		e.NewSink = func() (Sink, error) { return NewAplaySink(e.AplayPath) }
	}
	if e.NewDecoder == nil {
		e.NewDecoder = NewOpusDecoder
	}
	sk, err := e.NewSink()
	if err != nil {
		e.fail(s, fmt.Errorf("запуск вывода: %w", err))
		return
	}
	s.setSink(sk)
	if s.aborted.Load() { // вытеснили, пока aplay запускался
		sk.Abort()
		return
	}

	e.decMu.Lock()
	defer e.decMu.Unlock()
	if s.aborted.Load() { // пока ждали декодер, ответ успели вытеснить
		sk.Abort()
		return
	}
	var an *spectrum.Analyzer
	var stream uint64
	emit := func(k int, db []float32) {
		if e.OnSpectrum != nil {
			e.OnSpectrum(spectrum.Event{Kind: k, Source: spectrum.SourceVoice, Stream: stream, DB: db, At: time.Now()})
		}
	}
	if e.OnSpectrum != nil {
		an = spectrum.New(func(db []float32) { emit(spectrum.KindData, db) })
	}
	e.curTap = func(pcm []byte) {
		if an != nil {
			an.WriteMono(pcm)
		}
	}
	if e.dec == nil {
		d, err := e.NewDecoder(func(pcm []byte) { e.curTap(pcm) })
		if err != nil {
			sk.Abort()
			e.fail(s, fmt.Errorf("запуск декодера: %w", err))
			return
		}
		e.dec = d
	}
	dec := e.dec
	dec.Reset()

	playing, started := false, false
	finish := func(aborted bool) {
		if aborted || s.aborted.Load() {
			sk.Abort()
		} else {
			sk.Finish()
		}
		if started {
			emit(spectrum.KindStop, nil)
		}
	}
	for o := range s.ops {
		if s.aborted.Load() {
			finish(true)
			return
		}
		if o.kind == opEOS {
			break
		}
		if !started && an != nil { // поток спектра начинается с первым чанком (у mediad — чуть раньше PLAYING)
			started = true
			stream = uint64(clock.Mono())
			emit(spectrum.KindStart, nil)
		}
		err := dec.Write(o.data, func(pcm []byte) error {
			if _, err := sk.Write(pcm); err != nil {
				return err
			}
			if an != nil {
				an.WriteMono(pcm)
			}
			s.played.Add(uint64(len(pcm)))
			if !playing {
				playing = true
				s.startAt.Store(time.Now().UnixNano())
				if e.OnPlaying != nil {
					e.OnPlaying(s.id)
				}
			}
			return nil
		})
		if err != nil {
			if s.aborted.Load() {
				finish(true)
				return
			}
			if !playing {
				sk.Abort()
				if started {
					emit(spectrum.KindStop, nil)
				}
				e.fail(s, fmt.Errorf("декодирование чанка: %w", err))
				return
			}
			e.logf("голос %s: чанк пропущен: %v", s.id, err) // посреди ответа портить его целиком незачем
		}
	}
	if s.aborted.Load() {
		finish(true)
		return
	}
	finish(false)
	if s.aborted.Load() {
		return
	}
	e.mu.Lock()
	if e.cur == s {
		e.cur = nil
	}
	e.mu.Unlock()
	if e.OnEnded != nil {
		e.OnEnded(s.id)
	}
}
