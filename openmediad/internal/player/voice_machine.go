package player

import (
	"fmt"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

// Голос Алисы в автомате. Платформа присылает ответ тремя видами команд голосового плеера: voice-start (дескриптор), один или
// несколько voice-chunk (самостоятельные потоки Ogg/Opus) и voice-eos; после конца воспроизведения шлёт release. Автомат принимает их,
// отдаёт звук VoiceBackend и сам выпускает события в том порядке и с теми задержками, что у mediad (тайминги — по 53 ответам
// из записей: STARTING через ≈29 мс после voice-start, PLAYING через ≈50 мс после первого чанка, flag через ≈263 мс после PLAYING,
// progress каждые ≈1068 мс, ENDED после конца звука; позиции в flag и ENDED — позиция последнего progress).
const (
	voiceStartingDelay = 25 * time.Millisecond
	voiceFlagDelay     = 260 * time.Millisecond
	voiceProgressEvery = 1068 * time.Millisecond

	// voiceOpenTimeout — сколько ждём первого звука после первого чанка, прежде чем вернуть ответ настоящему mediad:
	// молчащая Алиса хуже любого отката.
	voiceOpenTimeout = 3 * time.Second
)

// VoiceBackend — звук голоса Алисы. Вызовы быстрые, блокировок машины не берут; результаты возвращаются методами Machine:
// VoicePlaying (первый звук ушёл на динамик), VoiceEnded (всё доиграно), VoiceFailed (не получилось начать).
type VoiceBackend interface {
	Start(id string)             // подготовить вывод (поднять aplay) по voice-start
	Chunk(id string, ogg []byte) // кусок Ogg/Opus; ogg принадлежит бэкенду
	EOS(id string)               // данных больше не будет
	Abort(id string)             // оборвать звук сразу
	Pos(id string) uint64        // сколько миллисекунд звука передано выводу
}

type vstate uint8

const (
	vStarting vstate = iota
	vPlaying
	vEnded
)

type vinst struct {
	meta      PlainMeta
	raw       [][]byte // команды ответа по порядку (voice-start, voice-chunk…, voice-eos): откат отдаёт их mediad
	ctx       any
	st        vstate
	startMs   uint64
	stopMs    uint64
	vsid      string
	mark      string
	lastProg  uint64 // позиция последнего progress, мс
	seenProg  bool
	starting  bool // событие STARTING уже ушло
	chunks    int
	cancelers []func()
}

func (m *Machine) vafter(v *vinst, d time.Duration, f func()) {
	cancel := m.After(d, f)
	m.mu.Lock()
	v.cancelers = append(v.cancelers, cancel)
	m.mu.Unlock()
}

func (m *Machine) vstop(v *vinst) {
	for _, c := range v.cancelers {
		c()
	}
	v.cancelers = nil
}

// voiceCommand обрабатывает voice-start, voice-chunk и voice-eos. false — команда не наша (пропустить mediad).
func (m *Machine) voiceCommand(ctx any, k quasar.CommandKind, c quasar.AudioCommand) bool {
	c = append(quasar.AudioCommand(nil), c...) // буфер кадра принадлежит шиму
	id := c.Player().ID
	m.mu.Lock()
	m.init()
	if m.vforeign[id] {
		m.mu.Unlock()
		return false
	}
	switch k {
	case quasar.CmdVoiceStart:
		now := uint64(m.Now().UnixMilli())
		v := &vinst{meta: PlainMetaFrom(c), raw: [][]byte{c}, ctx: ctx, startMs: now, vsid: newVsid(m.Now()), mark: randString(6)}
		for oid, o := range m.voices { // новый ответ вытесняет прежний, если тот ещё не закончился
			if o.st != vEnded {
				m.vstop(o)
				m.Voice.Abort(oid)
			}
			delete(m.voices, oid)
		}
		m.voices[id] = v
		m.note("voice-start", id, "")
		m.mu.Unlock()
		m.Voice.Start(id)
		m.vafter(v, voiceStartingDelay, func() { m.voiceStarting(id) })
		return true
	case quasar.CmdVoiceChunk:
		v := m.voices[id]
		if v == nil {
			m.mu.Unlock()
			return false
		}
		v.raw = append(v.raw, c)
		v.chunks++
		first := v.chunks == 1
		ogg, _ := c.VoiceChunk()
		ogg = append([]byte(nil), ogg...)
		m.mu.Unlock()
		if first {
			m.vafter(v, voiceOpenTimeout, func() { m.voiceTimeout(id) })
		}
		m.Voice.Chunk(id, ogg)
		return true
	case quasar.CmdVoiceEOS:
		v := m.voices[id]
		if v == nil {
			m.mu.Unlock()
			return false
		}
		v.raw = append(v.raw, c)
		m.mu.Unlock()
		m.Voice.EOS(id)
		return true
	}
	m.mu.Unlock()
	return false
}

// voiceRelease — команда release голосового плеера. После ENDED это штатный конец ответа; раньше конца — прерывание.
func (m *Machine) voiceRelease(id string) bool {
	m.mu.Lock()
	m.init()
	if m.vforeign[id] {
		delete(m.vforeign, id)
		m.mu.Unlock()
		return false
	}
	v := m.voices[id]
	if v == nil {
		m.mu.Unlock()
		return false
	}
	m.vstop(v)
	delete(m.voices, id)
	var ev []byte
	if v.st != vEnded {
		v.stopMs = uint64(m.Now().UnixMilli())
		v.st = vEnded
		ev = m.voiceEvent(v, 1, 5, v.lastProg)
	}
	m.note("voice-release", id, "")
	m.mu.Unlock()
	if ev != nil {
		m.Voice.Abort(id)
		m.Emit(ev)
	}
	return true
}

func (m *Machine) voiceStarting(id string) {
	m.mu.Lock()
	v := m.voices[id]
	if v == nil || v.st != vStarting {
		m.mu.Unlock()
		return
	}
	v.starting = true
	ev := m.voiceEvent(v, 1, 1, 0)
	m.mu.Unlock()
	m.Emit(ev)
}

// VoicePlaying вызывает бэкенд, когда первый звук ответа ушёл на динамик.
func (m *Machine) VoicePlaying(id string) {
	m.mu.Lock()
	v := m.voices[id]
	if v == nil || v.st != vStarting {
		m.mu.Unlock()
		return
	}
	var first []byte
	if !v.starting { // звук пошёл раньше, чем сработал таймер STARTING: порядок событий должен остаться штатным
		v.starting = true
		first = m.voiceEvent(v, 1, 1, 0)
	}
	v.st = vPlaying
	st := m.voiceEvent(v, 1, 3, 0)
	pr := m.voiceEvent(v, 2, 3, 0) // первый progress: позиция 0, как у mediad
	v.seenProg = true
	m.note("voice-playing", id, "")
	m.mu.Unlock()
	if first != nil {
		m.Emit(first)
	}
	m.Emit(st)
	m.Emit(pr)
	m.vafter(v, voiceFlagDelay, func() { m.voiceFlag(id) })
	m.vafter(v, voiceProgressEvery, func() { m.voiceProgress(id) })
}

func (m *Machine) voiceFlag(id string) {
	m.mu.Lock()
	v := m.voices[id]
	if v == nil || v.st != vPlaying {
		m.mu.Unlock()
		return
	}
	ev := m.voiceEvent(v, 6, 3, v.lastProg)
	m.mu.Unlock()
	m.Emit(ev)
}

func (m *Machine) voiceProgress(id string) {
	m.mu.Lock()
	v := m.voices[id]
	if v == nil || v.st != vPlaying {
		m.mu.Unlock()
		return
	}
	pos := m.Voice.Pos(id)
	v.lastProg = pos
	ev := m.voiceEvent(v, 2, 3, pos)
	m.mu.Unlock()
	m.Emit(ev)
	m.vafter(v, voiceProgressEvery, func() { m.voiceProgress(id) })
}

// VoiceEnded вызывает бэкенд, когда ответ доигран до конца и вывод закрыт: ENDED с позицией последнего progress.
func (m *Machine) VoiceEnded(id string) {
	m.mu.Lock()
	v := m.voices[id]
	if v == nil || v.st == vEnded {
		m.mu.Unlock()
		return
	}
	m.vstop(v)
	v.st = vEnded
	v.stopMs = uint64(m.Now().UnixMilli())
	ev := m.voiceEvent(v, 1, 5, v.lastProg)
	m.note("voice-ended", id, fmt.Sprintf("звук закончился, позиция %d мс", v.lastProg))
	m.mu.Unlock()
	m.Emit(ev)
}

// VoiceFailed вызывает бэкенд (или таймер), когда ответ не удалось начать воспроизводить. Пока звука не было, все команды
// ответа уходят настоящему mediad и он говорит сам; остальные команды этого плеера тоже идут ему. Если звук уже шёл,
// ответ просто считается законченным.
func (m *Machine) VoiceFailed(id string, err error) {
	m.mu.Lock()
	v := m.voices[id]
	if v == nil || v.st == vEnded {
		m.mu.Unlock()
		return
	}
	m.logf("голос %s: %v", id, err)
	m.note("voice-failed", id, err.Error())
	m.vstop(v)
	if v.st == vPlaying {
		m.mu.Unlock()
		m.VoiceEnded(id)
		return
	}
	raw, ctx := v.raw, v.ctx
	delete(m.voices, id)
	m.vforeign[id] = true
	m.mu.Unlock()
	m.Voice.Abort(id)
	if m.Forward != nil {
		for _, b := range raw {
			m.Forward(ctx, b)
		}
	}
}

func (m *Machine) voiceTimeout(id string) {
	m.mu.Lock()
	v := m.voices[id]
	stuck := v != nil && v.st == vStarting
	m.mu.Unlock()
	if stuck {
		m.VoiceFailed(id, fmt.Errorf("ответ не заиграл за %s", voiceOpenTimeout))
	}
}

// voiceEvent собирает событие голосового плеера. Вызывать под m.mu.
func (m *Machine) voiceEvent(v *vinst, kind, st, posMs uint64) []byte {
	s := PlainSnap{
		Kind: quasar.EventKind(kind), State: quasar.PlayState(st),
		PosMs: posMs, Idx: posMs / 1000, UUID: newUUID(),
		Start: v.startMs, Stop: v.stopMs, Vsid: v.vsid, Mark: v.mark, RSS: syntheticRSS,
	}
	if kind != 1 || st == 5 { // у STARTING и PLAYING нет ни длительности 0, ни монотонного времени
		s.Has6, s.Mono = true, m.Mono()
	}
	switch kind {
	case 2:
		s.HasT9, s.T9 = true, v.seenProg
	case 6:
		s.HasT17, s.T17 = true, true
	}
	if posMs >= 1000 { // played_sec появляется, когда проиграна хотя бы секунда
		s.HasPlayed, s.PlayedSec = true, posMs/1000
	}
	return v.meta.Event(s)
}

// syntheticRSS — rss_mem в diag наших событий; по этой константе их отличают от настоящих в разборе записей.
const syntheticRSS = 33000

var _ = pb.Has
