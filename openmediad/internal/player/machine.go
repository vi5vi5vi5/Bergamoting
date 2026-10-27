package player

import (
	"sync"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

// Тайминги штатного mediad, снятые с записей (msess1): через сколько после команды приходят события.
const (
	startingDelay = 14 * time.Millisecond  // play → STARTING
	warmPlaying   = 7 * time.Millisecond   // STARTING → PLAYING, если плеер уже готов (prefetch / кэш)
	pausedDelay   = 24 * time.Millisecond  // pause → PAUSED
	flagDelay     = 290 * time.Millisecond // PLAYING → первый flag
	progressEvery = 1060 * time.Millisecond

	// pauseCache — сколько плеер на паузе остаётся готовым к возобновлению. У mediad PCM закрывается примерно через
	// 30 минут простоя; больше одного плеера на паузе он не держит.
	pauseCache = 30 * time.Minute
)

// DuckGain — громкость музыки, пока у диалога аудиофокус (команда focus 39:1). Значение — приближение: в
// записях шины громкость плеера в событиях не меняется (поле 12 всегда 1.0), mediad приглушает музыку мимо неё.
const DuckGain = 0.25

// Backend — то, чем машина пользуется для настоящего звука. Вызовы быстрые, блокировок не берут.
// Результаты асинхронные и возвращаются методами Machine: Opened, Failed, Finished.
type Backend interface {
	Open(id string, t Track, offsetMs uint64) // начать загрузку (prefetch или play)
	Adopt(from, to string, offsetMs uint64)   // отдать открытый источник новому плееру (возобновление с паузы)
	Start(id string)                          // начать/продолжить вывод
	Pause(id string)                          // остановить вывод, источник и позицию держать
	Close(id string)                          // освободить всё
	Seek(id string, posMs uint64)             // перейти к позиции в открытом плеере
	Duck(gain float64)                        // громкость музыки
	Pos(id string) uint64                     // текущая позиция, мс
	Net(id string) (got, total uint64)        // скачано байт / длина потока (для diag)
}

// Track — описание трека для бэкенда.
type Track struct {
	ID      string // идентификатор трека (1.1)
	URL     string // основной URL (1.2)
	Alts    []string
	Key     []byte // AES-128 ключ потока (1.39, hex в команде)
	Bitrate uint64 // 1.45, кбит/с
	Title   string
	// GainDB — поправка громкости трека к целевой −14 LUFS (поле 21: пик и громкость трека). Формула — по смыслу
	// полей (в логах прошивки нет): громкие треки приглушаются, усиление не выше запаса до пика.
	GainDB float64
}

// NormTarget — целевая громкость нормализации, LUFS (поле 21.3 в событиях mediad).
const NormTarget = -14.0

// normGain считает поправку громкости по полю 21 дескриптора: f1 — истинный пик, дБ; f2 — громкость трека, LUFS.
func normGain(desc []byte) float64 {
	n, ok := pb.Bytes(desc, 21)
	if !ok {
		return 0
	}
	a, ok1 := pb.Find(n, 1)
	b, ok2 := pb.Find(n, 2)
	if !ok1 || !ok2 {
		return 0
	}
	peak, loud := float64frombits(a.U), float64frombits(b.U)
	g := NormTarget - loud
	if peak+g > 0 {
		g = -peak
	}
	if g > 12 {
		g = 12
	}
	return g
}

type state uint8

const (
	stNew      state = iota // prefetch получен, play ещё нет
	stStarting              // play получен
	stPlaying
	stPausing // команда pause принята, событие PAUSED ещё не отправлено
	stPaused
	stEnded
)

type inst struct {
	meta      Meta
	raw       []byte // последняя команда (prefetch/play) целиком: если бэкенд не справился, её получает mediad
	ctx       any    // откуда пришла команда (для Forward)
	st        state
	started   bool // событие STARTING уже ушло
	opened    bool
	failed    bool
	durMs     uint64
	startMs   uint64 // поле 5 события
	stopMs    uint64 // поле 7
	seenProg  bool
	vsid      string
	mark      string
	over      []pb.Field
	offset    uint64
	cancelers []func() // таймеры плеера: прогресс, flag, отложенные события
}

// Machine — модель набора плееров mediad: принимает команды platformd и выпускает события тем же языком.
type Machine struct {
	// Emit отправляет тело AudioClientEvent всем клиентам. Вызывается без удерживаемых блокировок машины,
	// возможно из разных горутин: реализация должна быть потокобезопасной.
	Emit func(body []byte)
	// Forward отдаёт команду настоящему mediad (откат: бэкенд не смог открыть поток). ctx — то, что передали в Handle.
	Forward func(ctx any, body []byte)
	// Now — текущее время; Mono — CLOCK_MONOTONIC в мс; After — запуск f через d (возвращает отмену).
	Now   func() time.Time
	Mono  func() uint64
	After func(d time.Duration, f func()) (cancel func())
	Back  Backend
	Logf  func(string, ...any)

	mu      sync.Mutex
	players map[string]*inst
	byTrack map[string]string // трек → id его плеера на паузе (кэш mediad)
	foreign map[string]bool   // плееры, которые ведёт настоящий mediad (откат): их команды не перехватываются
	ducked  bool
}

func (m *Machine) logf(f string, a ...any) {
	if m.Logf != nil {
		m.Logf(f, a...)
	}
}

func (m *Machine) init() {
	if m.players == nil {
		m.players = map[string]*inst{}
		m.byTrack = map[string]string{}
		m.foreign = map[string]bool{}
	}
}

// Handle разбирает команду плееру. Возвращает true, если команда обработана машиной и настоящему mediad
// пересылать её не нужно. Команды чужих плееров, голос и всё незнакомое возвращают false.
func (m *Machine) Handle(ctx any, c quasar.AudioCommand) bool {
	p := c.Player()
	switch k := c.Classify(); k {
	case quasar.CmdFocus:
		on, _ := pb.Uint(c, 39)
		m.Focus(on == 1)
		return false
	case quasar.CmdPlay, quasar.CmdPrefetch:
		if p.Kind != quasar.PlayerAudio || m.isForeign(p.ID) {
			return false
		}
		if k == quasar.CmdPrefetch {
			m.Prefetch(ctx, c)
		} else {
			return m.Play(ctx, c)
		}
		return true
	case quasar.CmdPause, quasar.CmdRelease, quasar.CmdCrossfade:
		if p.Kind != quasar.PlayerAudio || !m.owns(p.ID) {
			return false
		}
		switch k {
		case quasar.CmdPause:
			m.Pause(p.ID)
		case quasar.CmdRelease:
			m.Release(p.ID)
		default:
			m.Crossfade(c)
		}
		return true
	case quasar.CmdOther:
		if c.Reason() == "AUDIO_REWIND directive" && m.owns(p.ID) {
			if rw, ok := pb.Bytes(c, 13); ok {
				if f, ok := pb.Find(rw, 2); ok && f.Wire == pb.WireFixed64 {
					m.Seek(p.ID, int64(float64frombits(f.U)*1000))
				}
			}
			return true
		}
	}
	return false
}

func (m *Machine) isForeign(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.foreign[id]
}

func (m *Machine) owns(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.players[id]
	return ok
}

// TrackOf разбирает дескриптор команды в Track для бэкенда.
func TrackOf(desc []byte) Track {
	var t Track
	t.ID, _ = pb.Str(desc, 1)
	t.URL, _ = pb.Str(desc, 2)
	t.Bitrate, _ = pb.Uint(desc, 45)
	t.GainDB = normGain(desc)
	if k, ok := pb.Str(desc, 39); ok {
		t.Key = unhex(k)
	}
	if md, ok := pb.Bytes(desc, 9); ok {
		t.Title, _ = pb.Str(md, 1)
	}
	r := pb.NewReader(desc)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		if f.Num == 42 {
			t.Alts = append(t.Alts, string(f.B))
		}
	}
	return t
}

// Prefetch — команда prefetch: открыть трек заранее. События mediad при этом не шлёт.
func (m *Machine) Prefetch(ctx any, c quasar.AudioCommand) {
	c = append(quasar.AudioCommand(nil), c...) // буфер кадра принадлежит шиму и будет переиспользован
	m.mu.Lock()
	m.init()
	meta := MetaFrom(c, true)
	id := meta.Player.ID
	in := &inst{meta: meta, raw: append([]byte(nil), c...), ctx: ctx, st: stNew, vsid: newVsid(m.Now()), mark: randString(6)}
	m.players[id] = in
	off, _ := pb.Uint(meta.Desc, 3)
	m.mu.Unlock()
	m.Back.Open(id, TrackOf(meta.Desc), off)
}

// Play — команда play. Возвращает false, если запуск отдан настоящему mediad (открыть поток не удалось).
func (m *Machine) Play(ctx any, c quasar.AudioCommand) bool {
	c = append(quasar.AudioCommand(nil), c...) // буфер кадра принадлежит шиму и будет переиспользован
	m.mu.Lock()
	m.init()
	id := c.Player().ID
	in := m.players[id]
	if in != nil && in.failed {
		delete(m.players, id)
		m.foreign[id] = true
		m.mu.Unlock()
		return false
	}
	var adopt string
	if in == nil {
		track := c.Player().Stream
		var cached *inst
		if pid, ok := m.byTrack[track]; ok {
			cached = m.players[pid]
			adopt = pid
		}
		in = &inst{meta: MetaFrom(c, cached != nil), vsid: newVsid(m.Now()), mark: randString(6)}
		m.players[id] = in
		if cached != nil {
			// mediad возобновляет готовый плеер: в событиях остаётся дескриптор прежнего открытия (URL, зеркала, vsid).
			in.over = descURLFields(cached.meta.Desc, cached.over)
			in.vsid, in.mark = cached.vsid, cached.mark
			in.opened, in.durMs = cached.opened, cached.durMs
			cached.st = stEnded
			m.stopTimers(cached)
			delete(m.players, adopt)
			delete(m.byTrack, track)
		}
	} else {
		in.meta = MetaFrom(c, true) // дескриптор play точнее, чем у prefetch
	}
	in.raw, in.ctx = append([]byte(nil), c...), ctx
	in.offset, _ = pb.Uint(in.meta.Desc, 3)
	// Остальные плееры уступают: идущий или останавливаемый получает ENDED (после PAUSED, если трек тот же).
	var out [][]byte
	for oid, o := range m.players {
		if oid == id || o.st == stEnded || o.st == stNew || o.st == stPaused {
			continue
		}
		out = append(out, m.retire(o, o.meta.Player.Stream == in.meta.Player.Stream)...)
		delete(m.players, oid)
	}
	in.st = stStarting
	in.startMs = uint64(m.Now().UnixMilli()) + uint64(startingDelay/time.Millisecond)
	evict := m.evictPaused(in.meta.Player.Stream, id) // пользователь ушёл на другой трек: кэш паузы больше не нужен
	m.mu.Unlock()
	for _, eid := range evict {
		m.Back.Close(eid)
	}

	switch {
	case adopt != "":
		m.Back.Adopt(adopt, id, in.offset)
	case in.meta.Reused && in.opened:
		m.Back.Seek(id, in.offset)
	case !in.opened && !m.hasSource(id):
		m.Back.Open(id, TrackOf(in.meta.Desc), in.offset)
	}
	m.Back.Duck(m.gain())
	for _, e := range out {
		m.Emit(e)
	}
	m.after(in, startingDelay, func() { m.starting(id) })
	return true
}

func (m *Machine) hasSource(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.players[id]
	return in != nil && (in.opened || in.cancelers != nil || in.st == stNew)
}

func (m *Machine) gain() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ducked {
		return DuckGain
	}
	return 1
}

// retire завершает плеер, вытесненный новым play. Вызывать под m.mu; возвращает события к отправке.
func (m *Machine) retire(o *inst, sameTrack bool) (out [][]byte) {
	id := o.meta.Player.ID
	m.stopTimers(o)
	pos := m.Back.Pos(id)
	now := uint64(m.Now().UnixMilli())
	if o.st == stPausing && sameTrack {
		o.stopMs = now
		out = append(out, m.event(o, 1, 4, pos))
	}
	if o.stopMs == 0 {
		o.stopMs = now
	}
	out = append(out, m.event(o, 1, 5, pos))
	o.st = stEnded
	go m.Back.Close(id)
	return out
}

func (m *Machine) stopTimers(o *inst) {
	for _, c := range o.cancelers {
		c()
	}
	o.cancelers = nil
}

// after ставит таймер, привязанный к плееру (отменяется при остановке плеера). m.mu можно не держать.
func (m *Machine) after(in *inst, d time.Duration, f func()) {
	cancel := m.After(d, f)
	m.mu.Lock()
	in.cancelers = append(in.cancelers, cancel)
	m.mu.Unlock()
}

// starting — через startingDelay после play: STARTING, затем PLAYING (сразу, если источник уже готов).
func (m *Machine) starting(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stStarting {
		m.mu.Unlock()
		return
	}
	in.started = true
	ev := m.event(in, 1, 1, in.offset)
	ready := in.opened
	m.mu.Unlock()
	m.Emit(ev)
	m.Back.Start(id)
	if ready {
		m.after(in, warmPlaying, func() { m.playing(id) })
	}
}

// Opened — бэкенд открыл поток: известна длительность, можно играть.
func (m *Machine) Opened(id string, durMs uint64) {
	m.mu.Lock()
	m.init()
	in := m.players[id]
	if in == nil {
		m.mu.Unlock()
		return
	}
	in.opened, in.durMs = true, durMs
	wait := in.st == stStarting && in.started
	m.mu.Unlock()
	if wait {
		m.playing(id)
	}
}

// Failed — бэкенд не смог открыть поток. До play это запоминается (play уйдёт настоящему mediad), после play
// команда пересылается mediad сразу, а наш плеер снимается.
func (m *Machine) Failed(id string, err error) {
	m.mu.Lock()
	m.init()
	in := m.players[id]
	if in == nil {
		m.mu.Unlock()
		return
	}
	m.logf("плеер %s: поток не открылся: %v", id, err)
	in.failed = true
	if in.st != stStarting && in.st != stPlaying {
		m.mu.Unlock()
		return
	}
	m.stopTimers(in)
	raw, ctx := in.raw, in.ctx
	delete(m.players, id)
	m.foreign[id] = true
	m.mu.Unlock()
	m.Back.Close(id)
	if m.Forward != nil && raw != nil {
		m.Forward(ctx, raw)
	}
}

func (m *Machine) playing(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stStarting || !in.started {
		m.mu.Unlock()
		return
	}
	in.st = stPlaying
	pos := m.Back.Pos(id)
	if pos < in.offset {
		pos = in.offset
	}
	st := m.event(in, 1, 3, pos)
	pr := m.event(in, 2, 3, pos)
	in.seenProg = true
	m.mu.Unlock()
	m.Emit(st)
	m.Emit(pr)
	m.after(in, flagDelay, func() { m.flagEvent(id) })
	m.after(in, progressEvery, func() { m.progress(id) })
}

func (m *Machine) progress(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPlaying {
		m.mu.Unlock()
		return
	}
	ev := m.event(in, 2, 3, m.Back.Pos(id))
	m.mu.Unlock()
	m.Emit(ev)
	m.after(in, progressEvery, func() { m.progress(id) })
}

func (m *Machine) flagEvent(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPlaying {
		m.mu.Unlock()
		return
	}
	ev := m.event(in, 6, 3, m.Back.Pos(id))
	m.mu.Unlock()
	m.Emit(ev)
}

// Pause — команда pause: поток замирает, через pausedDelay уходит PAUSED.
func (m *Machine) Pause(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPlaying && in.st != stStarting {
		m.mu.Unlock()
		return
	}
	m.stopTimers(in)
	in.st = stPausing
	m.mu.Unlock()
	m.Back.Pause(id)
	m.after(in, pausedDelay, func() { m.paused(id) })
}

func (m *Machine) paused(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPausing {
		m.mu.Unlock()
		return
	}
	in.st = stPaused
	in.stopMs = uint64(m.Now().UnixMilli())
	m.byTrack[in.meta.Player.Stream] = id
	ev := m.event(in, 1, 4, m.Back.Pos(id))
	evict := m.evictPaused("", id) // на паузе держим один плеер: самый свежий
	m.mu.Unlock()
	m.Emit(ev)
	for _, eid := range evict {
		m.Back.Close(eid)
	}
	m.after(in, pauseCache, func() { m.expire(id) })
}

// evictPaused снимает с учёта плееры на паузе, кроме id except и плееров трека keepTrack; возвращает их id, которые
// надо закрыть в бэкенде (вне блокировки). Вызывать под m.mu.
func (m *Machine) evictPaused(keepTrack, except string) (ids []string) {
	for pid, o := range m.players {
		if pid == except || o.st != stPaused || keepTrack != "" && o.meta.Player.Stream == keepTrack {
			continue
		}
		m.stopTimers(o)
		if m.byTrack[o.meta.Player.Stream] == pid {
			delete(m.byTrack, o.meta.Player.Stream)
		}
		delete(m.players, pid)
		ids = append(ids, pid)
	}
	return ids
}

// expire закрывает плеер, простоявший на паузе дольше pauseCache.
func (m *Machine) expire(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPaused {
		m.mu.Unlock()
		return
	}
	if m.byTrack[in.meta.Player.Stream] == id {
		delete(m.byTrack, in.meta.Player.Stream)
	}
	delete(m.players, id)
	m.mu.Unlock()
	m.Back.Close(id)
}

// Release — команда release: отмена prefetch либо освобождение плеера.
func (m *Machine) Release(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil {
		m.mu.Unlock()
		return
	}
	m.stopTimers(in)
	if m.byTrack[in.meta.Player.Stream] == id {
		delete(m.byTrack, in.meta.Player.Stream)
	}
	delete(m.players, id)
	m.mu.Unlock()
	m.Back.Close(id)
}

// Crossfade — команда crossfade: с этого момента mediad показывает в дескрипторе поля 35 и 46.
func (m *Machine) Crossfade(c quasar.AudioCommand) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.players[c.Player().ID]
	cf, ok := pb.Bytes(c, 50)
	if in == nil || !ok {
		return
	}
	a, _ := pb.Uint(cf, 1)
	z, _ := pb.Uint(cf, 3)
	f35 := pb.AppendUint(nil, 1, 1)
	f35 = pb.AppendBytes(f35, 2, pb.AppendUint(pb.AppendUint(nil, 3, a), 4, z))
	in.over = setOver(in.over, pb.Field{Num: 35, Wire: pb.WireBytes, B: f35}, pb.Field{Num: 46, Wire: pb.WireVarint, U: a})
}

// Seek — относительная перемотка (AUDIO_REWIND): позиция сдвигается сразу и сообщается дважды (как у mediad),
// дальше platformd пришлёт pause и play со смещением.
func (m *Machine) Seek(id string, deltaMs int64) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPlaying {
		m.mu.Unlock()
		return
	}
	pos := int64(m.Back.Pos(id)) + deltaMs
	if pos < 0 {
		pos = 0
	}
	in.seenProg = false
	ev := m.event(in, 2, 3, uint64(pos))
	m.mu.Unlock()
	m.Back.Seek(id, uint64(pos))
	m.Emit(ev)
	m.Emit(ev)
}

// Focus — аудиофокус диалога: true приглушает музыку, false возвращает громкость; flag сообщает о смене.
func (m *Machine) Focus(on bool) {
	m.mu.Lock()
	m.init()
	if m.ducked == on {
		m.mu.Unlock()
		return
	}
	m.ducked = on
	var playing []*inst
	for _, in := range m.players {
		if in.st == stPlaying {
			playing = append(playing, in)
		}
	}
	g := 1.0
	if on {
		g = DuckGain
	}
	m.mu.Unlock()
	m.Back.Duck(g)
	for _, in := range playing {
		id := in.meta.Player.ID
		m.after(in, 300*time.Millisecond, func() { m.flagEvent(id) })
	}
}

// Finished — трек доигран до конца: ENDED с позицией, равной длительности.
func (m *Machine) Finished(id string) {
	m.mu.Lock()
	in := m.players[id]
	if in == nil || in.st != stPlaying {
		m.mu.Unlock()
		return
	}
	m.stopTimers(in)
	in.st = stEnded
	in.stopMs = uint64(m.Now().UnixMilli())
	ev := m.event(in, 1, 5, in.durMs)
	delete(m.players, id)
	m.mu.Unlock()
	m.Emit(ev)
	m.Back.Close(id)
}

// event собирает событие плеера. Вызывать под m.mu.
func (m *Machine) event(in *inst, kind, st, posMs uint64) []byte {
	s := Snap{
		Kind: quasar.EventKind(kind), State: quasar.PlayState(st),
		PosMs: posMs, Idx: posMs / 1000, UUID: newUUID(),
		Vsid: in.vsid, Start: in.startMs, Stop: in.stopMs,
		Over: in.over,
	}
	if in.durMs > 0 {
		s.HasDur, s.DurS = true, (in.durMs+500)/1000
		s.Mono = m.Mono()
	}
	switch kind {
	case 2:
		s.HasT9, s.T9 = true, in.seenProg
		in.seenProg = true
	case 6:
		s.HasT17, s.T17 = true, !m.ducked
	}
	got, total := m.Back.Net(in.meta.Player.ID)
	s.Diag = Diag(in.mark, in.vsid, got, total, st == 5, posMs/1000)
	return in.meta.Event(s)
}

func setOver(old []pb.Field, add ...pb.Field) []pb.Field {
	var out []pb.Field
	for _, f := range old {
		keep := true
		for _, a := range add {
			if a.Num == f.Num {
				keep = false
			}
		}
		if keep {
			out = append(out, f)
		}
	}
	return append(out, add...)
}

// descURLFields — поля 2 и 42 дескриптора открытого плеера (без суффикса vsid): их наследует возобновлённый плеер.
func descURLFields(desc []byte, prev []pb.Field) []pb.Field {
	out := append([]pb.Field(nil), prev...)
	var u2, u42 []pb.Field
	r := pb.NewReader(desc)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		switch f.Num {
		case 2:
			u2 = append(u2, f)
		case 42:
			u42 = append(u42, f)
		}
	}
	if len(u2) > 0 {
		out = setOver(out, u2...)
	}
	if len(u42) > 0 {
		out = setOver(out, u42...)
	}
	return out
}
