package player

import (
	"fmt"
	"strings"
	"time"

	"openmediad/internal/quasar"
)

// Плеер файлов (вид 4): звонок таймера и будильника играют локальный файл /system/vendor/quasar/sounds/common/timer.wav
// (по расширению «wav», по содержимому MP3) по кругу, пока платформа не пришлёт release. Команды: старт (`1:{4:4 19:0 25:путь
// 50:0}`, плеер `{1:4, 2:uuid, 4:1}`) и release. Автомат отдаёт звук FileBackend и выпускает события в порядке и с задержками mediad
// (по записям: 9 звонков, 908 событий): STARTING через ≈27 мс, затем progress в состоянии STARTING, PLAYING и первый progress
// (длительность файла становится известна), flag, progress примерно раз в 1,05 с. Спектра у плеера файлов нет.
const (
	fileStartingDelay = 25 * time.Millisecond
	fileFlagDelay     = 240 * time.Millisecond
	fileProgressEvery = 1050 * time.Millisecond
	fileOpenTimeout   = 3 * time.Second
)

// FileBackend — звук плеера файлов. Результаты возвращаются методами Machine: FilePlaying, FileFailed.
type FileBackend interface {
	Start(id, path string)    // раскодировать файл и играть по кругу
	Stop(id string)           // оборвать сразу
	Pos(id string) uint64     // позиция внутри текущего прохода файла, мс
	Elapsed(id string) uint64 // сколько всего звучит, мс
}

// DefaultFileOK — какие файлы шим берёт на себя: только системные звуки колонки. Всё остальное идёт настоящему mediad.
func DefaultFileOK(path string) bool {
	const dir = "/system/vendor/quasar/sounds/"
	return strings.HasPrefix(path, dir) && !strings.Contains(path, "..") && !strings.ContainsAny(path, " \t\n\"'`$;|&<>\\")
}

type finst struct {
	meta      PlainMeta
	path      string
	raw       []byte // стартовая команда: если бэкенд не справился (или шим останавливается), её получает mediad
	ctx       any
	st        vstate
	startMs   uint64
	stopMs    uint64
	vsid      string
	mark      string
	lastProg  uint64
	seenProg  bool   // первый progress в состоянии PLAYING уже ушёл: у него поле 9 = 0, у следующих 1
	wrapped   bool   // файл начался заново после прошлого progress: у этого progress поле 9 снова 0
	starting  bool   // событие STARTING уже ушло
	durMs     uint64 // длительность файла, мс (0 — ещё неизвестна)
	durKnown  bool   // поле 3.6 уже несёт длительность (до первого progress в PLAYING там 0)
	bitrate   uint64
	bitKnown  bool // diag bitrate появляется после первого progress в PLAYING
	cancelers []func()
}

// fafter запускает fn через d и запоминает отмену. Если звонок уже снят (release пришёл между вызовами), таймер отменяется сразу.
func (m *Machine) fafter(id string, f *finst, d time.Duration, fn func()) {
	cancel := m.After(d, fn)
	m.mu.Lock()
	if m.files[id] == f {
		f.cancelers = append(f.cancelers, cancel)
		cancel = nil
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Machine) fstop(f *finst) {
	for _, c := range f.cancelers {
		c()
	}
	f.cancelers = nil
}

// fileStart обрабатывает стартовую команду плеера файлов. false — файл не из разрешённых, одноразовый (играет mediad)
// или плеер отдан mediad.
func (m *Machine) fileStart(ctx any, c quasar.AudioCommand) bool {
	path, _ := c.FilePath()
	ok := m.FileOK
	if ok == nil {
		ok = DefaultFileOK
	}
	if !ok(path) {
		return false
	}
	if !c.FileLoops() { // системный звук (активация, щелчок громкости): играет один раз, конец сообщает плеер, release не придёт
		m.note("file-once", c.Player().ID, path)
		return false
	}
	c = append(quasar.AudioCommand(nil), c...) // буфер кадра принадлежит шиму
	id := c.Player().ID
	m.mu.Lock()
	m.init()
	if m.fforeign[id] {
		m.mu.Unlock()
		return false
	}
	if m.files[id] != nil { // платформа повторила стартовую команду: звонок уже идёт, второй звук не нужен
		m.note("file-start", id, "повторная команда, звонок уже идёт")
		m.mu.Unlock()
		return true
	}
	f := &finst{meta: PlainMetaFrom(c), path: path, raw: c, ctx: ctx, startMs: uint64(m.Now().UnixMilli()), vsid: newVsid(m.Now()), mark: randString(6)}
	m.files[id] = f
	m.note("file-start", id, path)
	m.mu.Unlock()
	m.File.Start(id, path)
	m.fafter(id, f, fileStartingDelay, func() { m.fileStarting(id) })
	m.fafter(id, f, fileOpenTimeout, func() { m.fileTimeout(id) })
	return true
}

// fileRelease — команда release плеера файлов: останавливает звонок и сообщает платформе ENDED.
func (m *Machine) fileRelease(id string) bool {
	m.mu.Lock()
	m.init()
	if m.fforeign[id] {
		delete(m.fforeign, id)
		m.mu.Unlock()
		return false
	}
	f := m.files[id]
	if f == nil {
		m.mu.Unlock()
		return false
	}
	m.fstop(f)
	delete(m.files, id)
	var ev []byte
	if f.st != vEnded {
		f.stopMs = uint64(m.Now().UnixMilli())
		f.st = vEnded
		ev = m.fileEvent(id, f, 1, 5, f.lastProg)
	}
	m.note("file-release", id, "")
	m.mu.Unlock()
	m.File.Stop(id)
	if ev != nil {
		m.Emit(ev)
	}
	return true
}

func (m *Machine) fileStarting(id string) {
	m.mu.Lock()
	f := m.files[id]
	if f == nil || f.st != vStarting || f.starting {
		m.mu.Unlock()
		return
	}
	f.starting = true
	ev := m.fileEvent(id, f, 1, 1, 0)
	m.mu.Unlock()
	m.Emit(ev)
}

// FilePlaying вызывает бэкенд, когда файл раскодирован и первый звук ушёл на динамик.
func (m *Machine) FilePlaying(id string, durMs, bitrate uint64) {
	m.mu.Lock()
	f := m.files[id]
	if f == nil || f.st != vStarting {
		m.mu.Unlock()
		return
	}
	f.durMs, f.bitrate = durMs, bitrate
	var evs [][]byte
	if !f.starting { // звук пошёл раньше таймера STARTING: порядок событий штатный в любом случае
		f.starting = true
		evs = append(evs, m.fileEvent(id, f, 1, 1, 0))
	}
	evs = append(evs, m.fileEvent(id, f, 2, 1, 0)) // progress в состоянии STARTING: длительность пока 0
	f.st = vPlaying
	evs = append(evs, m.fileEvent(id, f, 1, 3, 0)) // PLAYING: длительность всё ещё 0
	f.durKnown = true
	evs = append(evs, m.fileEvent(id, f, 2, 3, 0)) // первый progress: длительность известна, поле 9 = 0
	f.seenProg, f.bitKnown = true, true
	m.note("file-playing", id, fmt.Sprintf("длительность %d мс", durMs))
	m.mu.Unlock()
	for _, e := range evs {
		m.Emit(e)
	}
	m.fafter(id, f, fileFlagDelay, func() { m.fileFlag(id) })
	m.fafter(id, f, fileProgressEvery, func() { m.fileProgress(id) })
}

func (m *Machine) fileFlag(id string) {
	m.mu.Lock()
	f := m.files[id]
	if f == nil || f.st != vPlaying {
		m.mu.Unlock()
		return
	}
	ev := m.fileEvent(id, f, 6, 3, f.lastProg)
	m.mu.Unlock()
	m.Emit(ev)
}

func (m *Machine) fileProgress(id string) {
	m.mu.Lock()
	f := m.files[id]
	if f == nil || f.st != vPlaying {
		m.mu.Unlock()
		return
	}
	pos := m.File.Pos(id)
	if pos < f.lastProg { // файл начался заново: у первого progress нового прохода поле 9 = 0
		f.wrapped = true
	}
	f.lastProg = pos
	ev := m.fileEvent(id, f, 2, 3, pos)
	f.wrapped = false
	m.mu.Unlock()
	m.Emit(ev)
	m.fafter(id, f, fileProgressEvery, func() { m.fileProgress(id) })
}

// FileFailed вызывает бэкенд (или таймер), когда звонок не удалось начать: стартовая команда уходит настоящему mediad,
// и release этого плеера тоже. Если звук уже шёл, звонок считается законченным.
func (m *Machine) FileFailed(id string, err error) {
	m.mu.Lock()
	f := m.files[id]
	if f == nil || f.st == vEnded {
		m.mu.Unlock()
		return
	}
	m.logf("плеер файлов %s: %v", id, err)
	m.note("file-failed", id, err.Error())
	m.fstop(f)
	if f.st == vPlaying {
		f.st = vEnded
		f.stopMs = uint64(m.Now().UnixMilli())
		ev := m.fileEvent(id, f, 1, 5, f.lastProg)
		m.mu.Unlock()
		m.Emit(ev)
		return
	}
	raw, ctx := f.raw, f.ctx
	delete(m.files, id)
	m.fforeign[id] = true
	m.mu.Unlock()
	m.File.Stop(id)
	if m.Forward != nil {
		m.Forward(ctx, raw)
	}
}

func (m *Machine) fileTimeout(id string) {
	m.mu.Lock()
	f := m.files[id]
	stuck := f != nil && f.st == vStarting
	m.mu.Unlock()
	if stuck {
		m.FileFailed(id, fmt.Errorf("звонок не заиграл за %s", fileOpenTimeout))
	}
}

// fileEvent собирает событие плеера файлов. Вызывать под m.mu.
func (m *Machine) fileEvent(id string, f *finst, kind, st, posMs uint64) []byte {
	s := PlainSnap{
		Kind: quasar.EventKind(kind), State: quasar.PlayState(st),
		PosMs: posMs, Idx: posMs / 1000, UUID: newUUID(),
		Start: f.startMs, Stop: f.stopMs, Vsid: f.vsid, Mark: f.mark, RSS: syntheticRSS,
	}
	if !(kind == 1 && st == 1) { // у STARTING-как-смены-состояния нет ни длительности, ни монотонного времени
		s.Has6, s.Mono = true, m.Mono()
		if f.durKnown {
			s.Dur6 = (f.durMs + 500) / 1000
		}
	}
	if f.bitKnown && f.bitrate > 0 && (kind == 6 || kind == 2 || st == 5) {
		s.Bitrate = f.bitrate
	}
	switch kind {
	case 2:
		s.HasT9, s.T9 = true, f.seenProg && !f.wrapped
	case 6:
		s.HasT17, s.T17 = true, true
	}
	if el := m.File.Elapsed(id); el >= 1000 { // played_sec — полных секунд звучания
		s.HasPlayed, s.PlayedSec = true, el/1000
	}
	return f.meta.Event(s)
}

// fileHand — звонок, который надо передать настоящему mediad при остановке шима.
type fileHand struct {
	id  string
	raw []byte
	ctx any
}

// handoffFiles снимает идущие звонки (под m.mu). Платформа считает звонок идущим и событий конца не ждёт: ENDED не шлём,
// стартовую команду после остановки звука получает настоящий mediad, и звонок продолжается у него (handoffFilesDone).
func (m *Machine) handoffFiles() (out []fileHand) {
	for id, f := range m.files {
		m.fstop(f)
		delete(m.files, id)
		m.note("handoff", id, "шим останавливается: звонок передан mediad")
		out = append(out, fileHand{id, f.raw, f.ctx})
	}
	return out
}

func (m *Machine) handoffFilesDone(hs []fileHand) {
	for _, h := range hs {
		m.File.Stop(h.id)
		if m.Forward != nil {
			m.Forward(h.ctx, h.raw)
		}
	}
}

// fileInfos — звонки для отладочной страницы.
func (m *Machine) fileInfos() (out []PlayerInfo) {
	for id, f := range m.files {
		name := "звонок: запускается"
		if f.st == vPlaying {
			name = "звонок: звучит"
		}
		out = append(out, PlayerInfo{ID: id[:min(8, len(id))], State: name, Title: f.path, PosMs: m.File.Pos(id), DurMs: f.durMs})
	}
	return out
}
