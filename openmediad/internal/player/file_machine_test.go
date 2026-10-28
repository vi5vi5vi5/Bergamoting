package player

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

const timerWav = "/system/vendor/quasar/sounds/common/timer.wav"

type fakeFile struct {
	mu      sync.Mutex
	calls   []string
	pos     uint64
	elapsed uint64
}

func (f *fakeFile) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeFile) Start(id, path string) {
	f.rec("start " + id + " " + path)
}
func (f *fakeFile) Stop(id string) { f.rec("stop " + id) }
func (f *fakeFile) Pos(string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pos
}
func (f *fakeFile) Elapsed(string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.elapsed
}
func (f *fakeFile) set(pos, elapsed uint64) {
	f.mu.Lock()
	f.pos, f.elapsed = pos, elapsed
	f.mu.Unlock()
}
func (f *fakeFile) has(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			return true
		}
	}
	return false
}
func (f *fakeFile) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func newFileRig(t *testing.T) (*rig, *fakeFile) {
	r := newRig(t)
	ff := &fakeFile{}
	r.m.File = ff
	return r, ff
}

func filePlayer(id string) []byte {
	b := pb.AppendUint(nil, 1, 4)
	b = pb.AppendString(b, 2, id)
	return pb.AppendUint(b, 4, 1)
}

func fileStartCmd(id, path string) quasar.AudioCommand {
	d := pb.AppendUint(nil, 4, 4)
	d = pb.AppendUint(d, 19, 0)
	d = pb.AppendString(d, 25, path)
	d = pb.AppendUint(d, 50, 0)
	return pb.AppendBytes(pb.AppendBytes(nil, 1, d), 22, filePlayer(id))
}

// fileOnceCmd — стартовая команда одноразового системного звука (звук после «Алиса», щелчок громкости): в дескрипторе 50:1.
func fileOnceCmd(id, path string) quasar.AudioCommand {
	d := pb.AppendUint(nil, 4, 4)
	d = pb.AppendUint(d, 19, 0)
	d = pb.AppendString(d, 25, path)
	d = pb.AppendUint(d, 50, 1)
	return pb.AppendBytes(pb.AppendBytes(nil, 1, d), 22, filePlayer(id))
}

func fileReleaseCmd(id string) quasar.AudioCommand {
	return pb.AppendString(pb.AppendBytes(nil, 22, filePlayer(id)), 31, "")
}

// fileRuleViolation — какие поля несёт событие плеера файлов в каждом положении. Таблица восстановлена по 908 событиям из
// записей (TestFileEventsMatchRecordings прогоняет по ней настоящие события) и применяется к тому, что выпускает автомат.
// first — это первый progress в состоянии PLAYING (до него bitrate и played_sec нет, поле 9 = 0).
func fileRuleViolation(sn PlainSnap, first bool) string {
	k, st := sn.Kind, sn.State
	switch {
	case k == 1 && st == 1: // STARTING: самое бедное событие
		if sn.Has6 || sn.Mono != 0 || sn.HasT9 || sn.HasT17 || sn.Bitrate != 0 || sn.HasPlayed {
			return "state/STARTING: лишние поля"
		}
	case k == 2 && st == 1: // progress в состоянии STARTING: длительность ещё 0, поле 9 = 0
		if !sn.Has6 || sn.Dur6 != 0 || sn.Mono == 0 || !sn.HasT9 || sn.T9 || sn.HasT17 || sn.Bitrate != 0 || sn.HasPlayed {
			return "progress/STARTING"
		}
	case k == 1 && st == 3: // PLAYING: длительность всё ещё 0
		if !sn.Has6 || sn.Dur6 != 0 || sn.Mono == 0 || sn.HasT9 || sn.HasT17 || sn.Bitrate != 0 || sn.HasPlayed {
			return "state/PLAYING"
		}
	case k == 6 && st == 3: // flag: битрейт уже есть, played_sec ещё нет
		if !sn.Has6 || sn.Dur6 == 0 || sn.Mono == 0 || sn.HasT9 || !sn.HasT17 || !sn.T17 || sn.Bitrate == 0 || sn.HasPlayed {
			return "flag/PLAYING"
		}
	case k == 2 && st == 3:
		if !sn.Has6 || sn.Dur6 == 0 || sn.Mono == 0 || !sn.HasT9 || sn.HasT17 {
			return "progress/PLAYING: базовые поля"
		}
		if first && (sn.T9 || sn.Bitrate != 0 || sn.HasPlayed) {
			return "первый progress/PLAYING: поле 9 = 0, битрейта и played_sec нет"
		}
		if !first && (sn.Bitrate == 0 || !sn.HasPlayed) {
			return "следующий progress/PLAYING: нужны bitrate и played_sec"
		}
	case k == 1 && st == 5: // ENDED
		if !sn.Has6 || sn.Dur6 == 0 || sn.Mono == 0 || sn.HasT9 || sn.HasT17 || sn.Bitrate == 0 || !sn.HasPlayed || sn.Stop == 0 {
			return "state/ENDED"
		}
	default:
		return fmt.Sprintf("неожиданное событие %v/%v", k, st)
	}
	return ""
}

func fileKinds(r *rig) (out []string) {
	for _, g := range r.events() {
		out = append(out, g.kind().String()+"/"+g.state().String())
	}
	return
}

func TestFileRingSequenceTimingsAndFieldRules(t *testing.T) {
	r, ff := newFileRig(t)
	if !r.m.Handle(nil, fileStartCmd("f1", timerWav)) {
		t.Fatal("старт звонка обрабатывается машиной")
	}
	if !ff.has("start f1 " + timerWav) {
		t.Fatalf("бэкенд не запущен: %v", ff.calls)
	}
	r.advance(10 * time.Millisecond)
	if len(r.events()) != 0 {
		t.Fatal("STARTING раньше ≈25 мс приходить не должен")
	}
	r.advance(80 * time.Millisecond) // звук пошёл на 90-й миллисекунде
	r.m.FilePlaying("f1", 9273, 64)
	ff.set(0, 240)
	r.advance(300 * time.Millisecond) // flag на 240-й мс: звучало меньше секунды, played_sec ещё нет
	ff.set(1050, 1050)                // что бэкенд покажет к очередному progress
	r.advance(750 * time.Millisecond)
	ff.set(2100, 2100)
	r.advance(1050 * time.Millisecond)
	ff.set(2500, 2500) // позиция уходит вперёд, но ENDED обязан нести позицию последнего progress
	r.advance(10 * time.Millisecond)
	if !r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("release звонка обрабатывается машиной")
	}
	want := []string{"state/STARTING", "progress/STARTING", "state/PLAYING", "progress/PLAYING", "flag/PLAYING", "progress/PLAYING", "progress/PLAYING", "state/ENDED"}
	if got := fileKinds(r); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("события %v, ждали %v", got, want)
	}
	ev := r.events()
	if ev[0].at != 25*time.Millisecond {
		t.Fatalf("STARTING в %v, ждали +25 мс", ev[0].at)
	}
	if ev[1].at != 90*time.Millisecond || ev[2].at != 90*time.Millisecond || ev[3].at != 90*time.Millisecond {
		t.Fatalf("progress(STARTING), PLAYING и первый progress должны идти в момент первого звука: %v %v %v", ev[1].at, ev[2].at, ev[3].at)
	}
	if d := ev[4].at - ev[2].at; d != 240*time.Millisecond {
		t.Fatalf("flag через %v после PLAYING, ждали 240 мс", d)
	}
	if d := ev[5].at - ev[3].at; d != 1050*time.Millisecond {
		t.Fatalf("progress через %v, ждали 1050 мс", d)
	}
	if d := ev[6].at - ev[5].at; d != 1050*time.Millisecond {
		t.Fatalf("progress через %v, ждали 1050 мс", d)
	}
	first := true
	for i, g := range ev {
		if g.player() != "f1" {
			t.Fatalf("чужой плеер %q", g.player())
		}
		sn := plainSnapFrom(g.body)
		isFirst := g.kind() == 2 && g.state() == 3 && first
		if g.kind() == 2 && g.state() == 3 {
			first = false
		}
		if v := fileRuleViolation(sn, isFirst); v != "" {
			t.Errorf("событие %d (%v/%v): %s", i, g.kind(), g.state(), v)
		}
		if sn.Idx != sn.PosMs/1000 {
			t.Errorf("событие %d: секунды %d при позиции %d мс", i, sn.Idx, sn.PosMs)
		}
		if sn.Dur6 != 0 && sn.Dur6 != 9 {
			t.Errorf("событие %d: длительность %d с, ждали 9 (9273 мс)", i, sn.Dur6)
		}
	}
	if p := plainSnapFrom(ev[5].body); p.PlayedSec != 1 || p.PosMs != 1050 || p.Bitrate != 64 || !p.T9 {
		t.Fatalf("второй progress: %+v", p)
	}
	last := ev[7]
	if last.pos() != ev[6].pos() || last.pos() != 2100 {
		t.Fatalf("ENDED.pos = %d, последний progress.pos = %d (ждали 2100)", last.pos(), ev[6].pos())
	}
	if p := plainSnapFrom(last.body); p.PlayedSec != 2 {
		t.Fatalf("ENDED.played_sec = %d, ждали 2 (звучало 2500 мс)", p.PlayedSec)
	}
	if !ff.has("stop f1") {
		t.Fatal("release должен остановить звук")
	}
	n := len(r.events())
	r.advance(5 * time.Second)
	if len(r.events()) != n {
		t.Fatal("после release события продолжают идти")
	}
}

func TestFileLoopRestartMakesProgressNinthFieldZeroAgain(t *testing.T) {
	r, ff := newFileRig(t)
	r.m.Handle(nil, fileStartCmd("f1", timerWav))
	r.advance(90 * time.Millisecond)
	r.m.FilePlaying("f1", 9273, 64)
	for i, p := range []uint64{1050, 2100, 3150, 4200, 5250, 6300, 7350, 8400, 180, 1230} {
		ff.set(p, uint64(i+1)*1050)
		r.advance(1050 * time.Millisecond)
	}
	var t9 []bool
	var pos []uint64
	for _, g := range r.events() {
		if g.kind() == 2 && g.state() == 3 {
			sn := plainSnapFrom(g.body)
			t9 = append(t9, sn.T9)
			pos = append(pos, sn.PosMs)
		}
	}
	// первый progress (поз. 0) — 0; дальше 1; progress сразу после перезапуска файла (поз. 180) — снова 0; следом опять 1
	want := []bool{false, true, true, true, true, true, true, true, true, false, true}
	if fmt.Sprint(t9) != fmt.Sprint(want) {
		t.Fatalf("поле 9 по progress: %v (позиции %v), ждали %v", t9, pos, want)
	}
}

func TestFileFastSoundStillSendsStartingFirst(t *testing.T) {
	r, _ := newFileRig(t)
	r.m.Handle(nil, fileStartCmd("f1", timerWav))
	r.m.FilePlaying("f1", 9273, 64) // раньше, чем сработал таймер STARTING
	r.advance(100 * time.Millisecond)
	got := fileKinds(r)
	if len(got) < 4 || got[0] != "state/STARTING" || got[1] != "progress/STARTING" || got[2] != "state/PLAYING" {
		t.Fatalf("события %v: STARTING обязан идти первым и один раз", got)
	}
	n := 0
	for _, k := range got {
		if k == "state/STARTING" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("STARTING %d раз", n)
	}
}

func TestFileFailureBeforeSoundForwardsStartToMediad(t *testing.T) {
	r, ff := newFileRig(t)
	start := fileStartCmd("f1", timerWav)
	r.m.Handle("ctx", start)
	r.advance(30 * time.Millisecond) // STARTING уже ушёл
	r.m.FileFailed("f1", errors.New("нет aplay"))
	if len(r.fwd) != 1 || !bytes.Equal(r.fwd[0], start) {
		t.Fatalf("mediad должен получить стартовую команду, получено %d команд", len(r.fwd))
	}
	if !ff.has("stop f1") {
		t.Fatal("звук шима должен быть остановлен")
	}
	n0 := ff.count()
	if r.m.Handle(nil, start) {
		t.Fatal("повторный старт отданного звонка идёт mediad: шим не вмешивается на полпути")
	}
	if ff.count() != n0 {
		t.Fatal("отданный звонок снова запущен в шиме")
	}
	if r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("release отданного звонка идёт mediad")
	}
	if n := len(r.events()); n != 1 {
		t.Fatalf("за звонок, который ведёт mediad, шим выпускает только уже ушедший STARTING, а выпустил %d событий", n)
	}
	r.advance(10 * time.Second)
	if len(r.events()) != 1 || len(r.fwd) != 1 {
		t.Fatal("после отката шим продолжает вмешиваться")
	}
}

func TestFileStartBufferIsCopied(t *testing.T) {
	r, _ := newFileRig(t)
	start := fileStartCmd("f1", timerWav)
	orig := append([]byte(nil), start...)
	r.m.Handle(nil, start)
	for i := range start { // буфер кадра шим переиспользует
		start[i] = 0
	}
	r.m.FileFailed("f1", errors.New("сбой"))
	if len(r.fwd) != 1 || !bytes.Equal(r.fwd[0], orig) {
		t.Fatal("в mediad ушла не копия стартовой команды")
	}
}

func TestFileNoSoundWithinTimeoutFallsBack(t *testing.T) {
	r, _ := newFileRig(t)
	start := fileStartCmd("f1", timerWav)
	r.m.Handle(nil, start)
	r.advance(2900 * time.Millisecond)
	if len(r.fwd) != 0 {
		t.Fatal("откат раньше таймаута")
	}
	r.advance(200 * time.Millisecond)
	if len(r.fwd) != 1 || !bytes.Equal(r.fwd[0], start) {
		t.Fatal("после 3 с без звука стартовая команда должна уйти mediad")
	}
}

func TestFileNoTimeoutOnceSoundStarted(t *testing.T) {
	r, ff := newFileRig(t)
	r.m.Handle(nil, fileStartCmd("f1", timerWav))
	r.advance(50 * time.Millisecond)
	r.m.FilePlaying("f1", 9273, 64)
	for i := uint64(1); i <= 10; i++ {
		ff.set(i*1050%9273, i*1050)
		r.advance(1050 * time.Millisecond)
	}
	if len(r.fwd) != 0 {
		t.Fatal("звонок уже звучит: откат не нужен")
	}
	if k := fileKinds(r); k[len(k)-1] != "progress/PLAYING" {
		t.Fatalf("события должны идти: %v", k)
	}
}

func TestFileFailureWhileRingingEndsIt(t *testing.T) {
	r, ff := newFileRig(t)
	r.m.Handle(nil, fileStartCmd("f1", timerWav))
	r.advance(50 * time.Millisecond)
	r.m.FilePlaying("f1", 9273, 64)
	ff.set(1050, 1500)
	r.advance(1100 * time.Millisecond)
	r.m.FileFailed("f1", errors.New("aplay умер"))
	k := fileKinds(r)
	if k[len(k)-1] != "state/ENDED" {
		t.Fatalf("события %v: звонок оборвался, платформа должна получить ENDED", k)
	}
	if len(r.fwd) != 0 {
		t.Fatal("звук уже шёл: стартовую команду mediad отдавать нельзя, получится второй звонок")
	}
	n := len(r.events())
	r.advance(3 * time.Second)
	if len(r.events()) != n {
		t.Fatal("после ENDED события продолжают идти")
	}
	if !r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("release после ENDED обрабатывает шим")
	}
	if len(r.events()) != n {
		t.Fatal("release после ENDED не должен порождать второй ENDED")
	}
}

func TestFileReleaseWhileStillStartingEndsAndCancelsTimers(t *testing.T) {
	r, ff := newFileRig(t)
	start := fileStartCmd("f1", timerWav)
	r.m.Handle(nil, start)
	r.advance(30 * time.Millisecond)
	if !r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("release обрабатывается машиной")
	}
	if !ff.has("stop f1") {
		t.Fatal("звук должен быть остановлен")
	}
	k := fileKinds(r)
	if k[len(k)-1] != "state/ENDED" {
		t.Fatalf("события %v", k)
	}
	r.advance(10 * time.Second)
	if len(r.fwd) != 0 {
		t.Fatal("снятый звонок не должен откатываться на mediad по таймеру")
	}
	r.m.FilePlaying("f1", 9273, 64) // поздний ответ бэкенда после release игнорируется
	if n := len(r.events()); n != len(k) {
		t.Fatalf("после release появились события: %d вместо %d", n, len(k))
	}
}

func TestFileRepeatedStartDoesNotDoubleTheRing(t *testing.T) {
	r, ff := newFileRig(t)
	start := fileStartCmd("f1", timerWav)
	r.m.Handle(nil, start)
	n := ff.count()
	if !r.m.Handle(nil, start) {
		t.Fatal("повторная стартовая команда не должна уходить mediad: получится второй звонок")
	}
	if ff.count() != n {
		t.Fatal("повтор запустил звук ещё раз")
	}
}

func TestFileOutsideSystemSoundsGoesToMediad(t *testing.T) {
	r, ff := newFileRig(t)
	for _, p := range []string{"/data/x.wav", "/system/vendor/quasar/sounds/../maind", "/system/vendor/quasar/sounds/a b.wav", "/system/vendor/quasar/sounds/x;y.wav"} {
		if r.m.Handle(nil, fileStartCmd("f1", p)) {
			t.Fatalf("%q не из системных звуков: команда должна идти mediad", p)
		}
	}
	if ff.count() != 0 {
		t.Fatal("бэкенд не должен трогать чужие файлы")
	}
	if r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("release чужого плеера идёт mediad")
	}
	r.m.FileOK = func(p string) bool { return p == "/data/x.wav" }
	if !r.m.Handle(nil, fileStartCmd("f2", "/data/x.wav")) {
		t.Fatal("FileOK разрешил файл: машина обязана его взять")
	}
}

// Звук после «Алиса» и щелчок громкости играют один раз, и конец сообщает сам плеер: release платформа не присылает. Шим,
// игравший их по кругу «до release», зациклил звук на колонке (проверка на слух 28.10.2026): теперь такие звуки играет mediad.
func TestFileOneShotSoundsStayWithMediad(t *testing.T) {
	r, ff := newFileRig(t)
	var notes []string
	r.m.Note = func(kind, id, detail string) { notes = append(notes, kind+" "+id+" "+detail) }
	for i, p := range []string{
		"/system/vendor/quasar/sounds/common/spotter_start.mp3",
		"/system/vendor/quasar/sounds/common/volume_sound.mp3",
	} {
		id := fmt.Sprintf("o%d", i)
		if r.m.Handle(nil, fileOnceCmd(id, p)) {
			t.Fatalf("%s: одноразовый звук должен идти mediad", p)
		}
		if r.m.Handle(nil, fileReleaseCmd(id)) {
			t.Fatalf("%s: release одноразового плеера идёт mediad", p)
		}
	}
	r.advance(5 * time.Second)
	if ff.count() != 0 {
		t.Fatalf("бэкенд не должен играть одноразовые звуки: %v", ff.calls)
	}
	if n := len(r.events()); n != 0 {
		t.Fatalf("шим не должен выпускать события чужого плеера: %d", n)
	}
	if len(notes) != 2 || notes[0][:9] != "file-once" {
		t.Fatalf("в журнал должны попасть оба звука: %v", notes)
	}
	if !r.m.Handle(nil, fileStartCmd("ring", timerWav)) { // звонок рядом по-прежнему берёт шим
		t.Fatal("звонок таймера (50:0) должен играть шим")
	}
}

func TestFileIgnoredWithoutBackend(t *testing.T) {
	r := newRig(t)
	if r.m.Handle(nil, fileStartCmd("f1", timerWav)) || r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("без бэкенда звонки идут mediad")
	}
}

func TestHandoffForwardsActiveRingToMediad(t *testing.T) {
	r, ff := newFileRig(t)
	start := fileStartCmd("f1", timerWav)
	r.m.Handle("ctx", start)
	r.advance(50 * time.Millisecond)
	r.m.FilePlaying("f1", 9273, 64)
	r.advance(500 * time.Millisecond)
	n := len(r.events())
	r.m.Handoff()
	if !ff.has("stop f1") {
		t.Fatal("звук шима должен быть остановлен")
	}
	if len(r.fwd) != 1 || !bytes.Equal(r.fwd[0], start) {
		t.Fatal("звонок должен продолжиться у mediad: ему отдаётся стартовая команда")
	}
	if len(r.events()) != n {
		t.Fatal("ENDED при передаче не нужен: платформа считает звонок идущим")
	}
	r.advance(5 * time.Second)
	if len(r.events()) != n {
		t.Fatal("после передачи события продолжают идти")
	}
	if r.m.Handle(nil, fileReleaseCmd("f1")) {
		t.Fatal("после передачи release этого звонка идёт mediad")
	}
}

func TestSnapshotShowsRing(t *testing.T) {
	r, ff := newFileRig(t)
	r.m.Handle(nil, fileStartCmd("ring-1234567890", timerWav))
	if s := r.m.Snapshot(); len(s) != 1 || s[0].State != "звонок: запускается" || s[0].Title != timerWav {
		t.Fatalf("снимок до звука: %+v", s)
	}
	r.advance(50 * time.Millisecond)
	r.m.FilePlaying("ring-1234567890", 9273, 64)
	ff.set(777, 777)
	if s := r.m.Snapshot(); len(s) != 1 || s[0].State != "звонок: звучит" || s[0].PosMs != 777 || s[0].DurMs != 9273 || s[0].ID != "ring-123" {
		t.Fatalf("снимок со звуком: %+v", s)
	}
}

func TestFileNonMP3HasNoBitrate(t *testing.T) {
	r, ff := newFileRig(t)
	r.m.Handle(nil, fileStartCmd("f1", "/system/vendor/quasar/sounds/common/bluetooth_turning_on_jingle.opus"))
	r.advance(50 * time.Millisecond)
	r.m.FilePlaying("f1", 1500, 0)
	ff.set(1050, 1050)
	r.advance(1100 * time.Millisecond)
	for _, g := range r.events() {
		if sn := plainSnapFrom(g.body); sn.Bitrate != 0 {
			t.Fatalf("у файла без известного битрейта diag не должен нести bitrate (%v/%v)", g.kind(), g.state())
		}
	}
}
