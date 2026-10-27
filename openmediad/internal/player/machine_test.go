package player

import (
	"errors"
	"math"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

func TestWarmStartSequenceAndTimings(t *testing.T) {
	r := newRig(t)
	if !r.m.Handle(nil, prefetchCmd("p1", "t1")) {
		t.Fatal("prefetch аудио-плеера должен обрабатываться машиной")
	}
	r.m.Opened("p1", 200_000)
	r.advance(900 * time.Millisecond)
	if n := len(r.events()); n != 0 {
		t.Fatalf("после prefetch событий быть не должно, а их %d", n)
	}
	if !r.m.Handle(nil, playCmd("p1", "t1", 0)) {
		t.Fatal("play должен обрабатываться машиной")
	}
	r.advance(2500 * time.Millisecond)

	ev := r.events()
	if ev[0].kind() != 1 || ev[0].state() != 1 || ev[0].at != 900*time.Millisecond+14*time.Millisecond { // 14 мс — по записи штатного mediad
		t.Fatalf("первое событие — STARTING через 14 мс после play, а не %v/%v в %v", ev[0].kind(), ev[0].state(), ev[0].at)
	}
	if ev[1].kind() != 1 || ev[1].state() != 3 || ev[1].at-ev[0].at != 7*time.Millisecond { // 7 мс — по записи
		t.Fatalf("PLAYING должен идти через 7 мс после STARTING: %v/%v +%v", ev[1].kind(), ev[1].state(), ev[1].at-ev[0].at)
	}
	if ev[2].kind() != 2 {
		t.Fatalf("вслед за PLAYING — первый progress, а не %v", ev[2].kind())
	}
	var prog []got
	flags := 0
	for _, g := range ev {
		switch g.kind() {
		case 2:
			prog = append(prog, g)
		case 6:
			flags++
		}
		if g.player() != "p1" {
			t.Fatalf("событие чужого плеера %q", g.player())
		}
		if pb.Validate(g.body) != nil {
			t.Fatal("событие не разбирается как protobuf")
		}
	}
	if flags != 1 {
		t.Fatalf("за 2,5 с ждём один flag, получено %d", flags)
	}
	if len(prog) != 3 {
		t.Fatalf("за 2,5 с ждём 3 progress (старт, +1,06 и +2,12 с), получено %d", len(prog))
	}
	if d := prog[2].pos() - prog[1].pos(); d < 1050 || d > 1070 {
		t.Fatalf("позиция между progress растёт на %d мс вместо ≈1060", d)
	}
}

func TestColdStartWaitsForOpen(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.advance(600 * time.Millisecond)
	if got := r.states(); len(got) != 1 || got[0] != "STARTING/p1" {
		t.Fatalf("пока поток не открыт, ждём только STARTING: %v", got)
	}
	if !r.be.has("open p1 t1") {
		t.Fatal("play без prefetch обязан открыть поток")
	}
	r.m.Opened("p1", 180_000)
	r.advance(10 * time.Millisecond)
	if got := r.states(); len(got) != 2 || got[1] != "PLAYING/p1" {
		t.Fatalf("после открытия потока ждём PLAYING: %v", got)
	}
}

func TestPauseThenResumeReusesPlayerAndURLs(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(5 * time.Second)
	r.m.Handle(nil, pauseCmd("p1", "t1"))
	r.advance(100 * time.Millisecond)
	st := r.states()
	if st[len(st)-1] != "PAUSED/p1" {
		t.Fatalf("после pause ждём PAUSED: %v", st)
	}
	pausedAt := r.events()[len(r.events())-1].pos()
	if pausedAt < 4900 || pausedAt > 5300 {
		t.Fatalf("позиция паузы %d мс, ждали ≈5000", pausedAt)
	}
	before := len(r.events())
	r.advance(20 * time.Second)
	if len(r.events()) != before {
		t.Fatal("на паузе события идти не должны")
	}
	// возобновление: новый id, тот же трек, смещение = позиция паузы
	r.m.Handle(nil, playCmd("p2", "t1", pausedAt))
	r.advance(100 * time.Millisecond)
	if !r.be.has("adopt p1→p2") {
		t.Fatalf("возобновление должно забрать готовый источник паузы: %v", r.be.calls)
	}
	var starting got
	for _, g := range r.events() {
		if g.player() == "p2" && g.state() == 1 && g.kind() == 1 {
			starting = g
		}
	}
	if starting.body == nil || starting.pos() != pausedAt {
		t.Fatalf("STARTING возобновления с позиции %d, получено %d", pausedAt, starting.pos())
	}
	d, _ := pb.Bytes(starting.body, 3)
	if pb.Has(d, 26) || !pb.Has(d, 30) {
		t.Fatal("для готового плеера дескриптор события теряет поле 26 и получает 30")
	}
}

func TestNextTrackEndsOldWithoutPaused(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(3 * time.Second)
	r.m.Handle(nil, pauseCmd("p1", "t1")) // «следующий»: pause и сразу play другого трека
	r.advance(5 * time.Millisecond)
	r.m.Handle(nil, playCmd("p2", "t2", 0))
	r.m.Opened("p2", 150_000)
	r.advance(200 * time.Millisecond)
	want := []string{"STARTING/p1", "PLAYING/p1", "ENDED/p1", "STARTING/p2", "PLAYING/p2"}
	st := r.states()
	if len(st) != len(want) {
		t.Fatalf("состояния %v, ждали %v", st, want)
	}
	for i := range want {
		if st[i] != want[i] {
			t.Fatalf("состояния %v, ждали %v", st, want)
		}
	}
}

func TestSameTrackReplaceGivesPausedThenEnded(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(3 * time.Second)
	r.m.Handle(nil, pauseCmd("p1", "t1")) // перемотка: pause и play того же трека с новым смещением
	r.advance(5 * time.Millisecond)
	r.m.Handle(nil, playCmd("p2", "t1", 63000))
	r.advance(5 * time.Millisecond)
	want := []string{"STARTING/p1", "PLAYING/p1", "PAUSED/p1", "ENDED/p1"}
	st := r.states()
	for i := range want {
		if i >= len(st) || st[i] != want[i] {
			t.Fatalf("состояния %v, ждали начало %v", st, want)
		}
	}
}

func TestFailureHandsCommandBackToMediad(t *testing.T) {
	r := newRig(t)
	play := playCmd("p1", "t1", 0)
	r.m.Handle("ctx", play)
	r.advance(50 * time.Millisecond)
	r.m.Failed("p1", errors.New("нет сети"))
	if len(r.fwd) != 1 || string(r.fwd[0]) != string(play) {
		t.Fatalf("команда play должна уйти настоящему mediad без изменений, отправлено %d", len(r.fwd))
	}
	if r.m.Handle(nil, pauseCmd("p1", "t1")) {
		t.Fatal("команды отданного mediad плеера не перехватываются")
	}
	if !r.be.closed["p1"] {
		t.Fatal("бэкенд обязан освободить упавший плеер")
	}
}

func TestPrefetchFailureFallsBackAtPlay(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, prefetchCmd("p1", "t1"))
	r.m.Failed("p1", errors.New("404"))
	if r.m.Handle(nil, playCmd("p1", "t1", 0)) {
		t.Fatal("play после неудачного prefetch должен уйти mediad (Handle=false)")
	}
	if len(r.events()) != 0 {
		t.Fatal("машина не должна выпускать события за плеер, который ведёт mediad")
	}
}

func TestIgnoresVoiceAndForwardsFocus(t *testing.T) {
	r := newRig(t)
	voice := pb.AppendBytes(nil, 22, pb.AppendUint(pb.AppendString(nil, 2, "v1"), 1, 2))
	voice = pb.AppendString(voice, 29, "OggS")
	if r.m.Handle(nil, voice) {
		t.Fatal("голос Алисы машина не трогает")
	}
	if r.m.Handle(nil, focusCmd(1)) {
		t.Fatal("focus надо переслать настоящему mediad, машина лишь приглушает музыку")
	}
	if r.be.gain != DuckGain {
		t.Fatalf("громкость во время диалога %v, ждали %v", r.be.gain, DuckGain)
	}
	r.m.Handle(nil, focusCmd(0))
	if r.be.gain != 1 {
		t.Fatalf("после focus 0 громкость должна вернуться, а она %v", r.be.gain)
	}
}

func TestFocusReportsFlag(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(2 * time.Second)
	n0 := len(r.events())
	r.m.Handle(nil, focusCmd(1))
	r.advance(400 * time.Millisecond)
	var flag *got
	for _, g := range r.events()[n0:] {
		if g.kind() == 6 {
			x := g
			flag = &x
		}
	}
	if flag == nil {
		t.Fatal("после смены фокуса ждём событие flag")
	}
	if v, ok := pb.Uint(flag.body, 17); !ok || v != 0 {
		t.Fatalf("flag при приглушении: поле 17 = %d (есть=%v), ждали 0", v, ok)
	}
}

func TestFinishedSendsEndedAtDuration(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 3_000)
	r.advance(500 * time.Millisecond)
	r.m.Finished("p1")
	last := r.events()[len(r.events())-1]
	if last.kind() != 1 || last.state() != 5 || last.pos() != 3000 {
		t.Fatalf("последнее событие %v/%v pos=%d, ждали ENDED на 3000", last.kind(), last.state(), last.pos())
	}
	n := len(r.events())
	r.advance(5 * time.Second)
	if len(r.events()) != n {
		t.Fatal("после конца трека события идти не должны")
	}
}

func TestSeekMovesPositionTwice(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(10 * time.Second)
	n0 := len(r.events())
	rw := pb.AppendFixed64(nil, 2, 0x404e000000000000) // 60.0 с
	cmd := pb.AppendBytes(nil, 13, rw)
	cmd = pb.AppendBytes(cmd, 22, playerOf("p1", "t1"))
	cmd = pb.AppendString(cmd, 52, "AUDIO_REWIND directive")
	if !r.m.Handle(nil, quasar.AudioCommand(cmd)) {
		t.Fatal("перемотка своего плеера обрабатывается машиной")
	}
	got := r.events()[n0:]
	if len(got) != 2 || got[0].kind() != 2 || got[0].pos() < 69900 || got[0].pos() > 70300 {
		t.Fatalf("после перемотки ждём 2 progress около 70000 мс, получено %d, pos=%d", len(got), got[0].pos())
	}
	if !r.be.has("seek p1") {
		t.Fatal("бэкенд должен получить Seek")
	}
}

func TestNormGainFromField21(t *testing.T) {
	desc := func(peak, loud float64) []byte {
		n := pb.AppendFixed64(nil, 1, math.Float64bits(peak))
		n = pb.AppendFixed64(n, 2, math.Float64bits(loud))
		return pb.AppendBytes(pb.AppendString(nil, 1, "t"), 21, n)
	}
	// значения реальных треков из записей: громкий «Hot N Cold» (пик +1,91, −6,48 LUFS) глушится на 7,52 дБ
	if g := TrackOf(desc(1.91, -6.48)).GainDB; math.Abs(g-(-7.52)) > 0.001 {
		t.Fatalf("поправка %.3f дБ, ждали −7,52", g)
	}
	// тихий трек не усиливается выше запаса до пика: пик −1 дБ, громкость −20 LUFS → хотелось бы +6, можно +1
	if g := TrackOf(desc(-1, -20)).GainDB; math.Abs(g-1) > 0.001 {
		t.Fatalf("тихий трек: %.3f дБ, ждали +1 (предел по пику)", g)
	}
	if g := TrackOf(pb.AppendString(nil, 1, "t")).GainDB; g != 0 {
		t.Fatalf("без поля 21 поправки нет, а получено %v", g)
	}
}

func TestPausedCacheKeepsOnlyOneAndExpires(t *testing.T) {
	r := newRig(t)
	pauseAt := func(id, track string) {
		r.m.Handle(nil, playCmd(id, track, 0))
		r.m.Opened(id, 200_000)
		r.advance(2 * time.Second)
		r.m.Handle(nil, pauseCmd(id, track))
		r.advance(100 * time.Millisecond)
	}
	pauseAt("p1", "t1")
	if r.be.closed["p1"] {
		t.Fatal("плеер на паузе должен оставаться в кэше")
	}
	// другой трек: кэш t1 больше не нужен
	r.m.Handle(nil, playCmd("p2", "t2", 0))
	r.m.Opened("p2", 200_000)
	r.advance(100 * time.Millisecond)
	if !r.be.closed["p1"] {
		t.Fatal("при запуске другого трека кэш паузы старого должен закрыться")
	}
	r.advance(time.Second)
	r.m.Handle(nil, pauseCmd("p2", "t2"))
	r.advance(100 * time.Millisecond)
	pauseAt("p3", "t3") // новый трек и пауза: старые паузы не накапливаются
	if !r.be.closed["p2"] {
		t.Fatal("на паузе держим один плеер: p2 должен закрыться, когда p3 встал на паузу")
	}
	if r.be.closed["p3"] {
		t.Fatal("свежий плеер на паузе закрываться не должен")
	}
	r.advance(29 * time.Minute)
	if r.be.closed["p3"] {
		t.Fatal("до истечения 30 минут кэш паузы жив")
	}
	r.advance(2 * time.Minute)
	if !r.be.closed["p3"] {
		t.Fatal("через 30 минут простоя плеер на паузе должен закрыться")
	}
	// возобновление после истечения — обычный холодный запуск
	r.m.Handle(nil, playCmd("p4", "t3", 5000))
	if r.be.has("adopt p3→p4") {
		t.Fatal("закрытый кэш усыновлять нельзя")
	}
}

func TestHandoffPausesPlayingOnly(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(4 * time.Second)
	r.m.Handle(nil, prefetchCmd("p2", "t2")) // предзагруженный, но не запущенный: события не нужны
	n0 := len(r.events())
	r.m.Handoff()
	got := r.events()[n0:]
	if len(got) != 1 || got[0].kind() != 1 || got[0].state() != 4 || got[0].player() != "p1" {
		t.Fatalf("после Handoff ждём одно событие PAUSED для p1, получено %d", len(got))
	}
	if p := got[0].pos(); p < 3900 || p > 4300 {
		t.Fatalf("позиция в PAUSED %d мс, ждали ≈4000", p)
	}
	if !r.be.has("pause p1") {
		t.Fatal("бэкенд должен остановить вывод")
	}
	if v, _ := pb.Uint(got[0].body, 7); v == 0 {
		t.Fatal("у PAUSED должно быть время остановки (поле 7)")
	}
	r.advance(5 * time.Second)
	if len(r.events()) != n0+1 {
		t.Fatal("после Handoff события прогресса идти не должны")
	}
	// повторный Handoff ничего не добавляет
	r.m.Handoff()
	if len(r.events()) != n0+1 {
		t.Fatal("плеер на паузе второй раз не паузится")
	}
}

// Регрессия: платформа присылает prefetch того же плеера второй раз перед концом трека. Раньше автомат забывал, что поток
// открыт, и следующий play висел на STARTING без звука (запись длинного прогона 27.10.2026).
func TestRepeatedPrefetchKeepsOpenedState(t *testing.T) {
	r := newRig(t)
	r.m.Handle(nil, prefetchCmd("p1", "t1"))
	r.m.Opened("p1", 200_000)
	r.advance(5 * time.Second)
	r.m.Handle(nil, prefetchCmd("p1", "t1")) // повтор
	r.advance(time.Second)
	r.m.Handle(nil, playCmd("p1", "t1", 0))
	r.advance(100 * time.Millisecond)
	want := []string{"STARTING/p1", "PLAYING/p1"}
	st := r.states()
	if len(st) != 2 || st[0] != want[0] || st[1] != want[1] {
		t.Fatalf("после повторного prefetch play должен давать быстрый старт: %v, ждали %v", st, want)
	}
	if n := countCalls(r.be, "open p1 t1"); n != 1 {
		t.Fatalf("поток открывался %d раз, ждали один", n)
	}
}

func TestStuckStartFallsBackToMediad(t *testing.T) {
	r := newRig(t)
	play := playCmd("p1", "t1", 0)
	r.m.Handle("ctx", play)
	r.advance(14 * time.Second)
	if len(r.fwd) != 0 {
		t.Fatal("до таймаута команду отдавать рано")
	}
	r.advance(2 * time.Second) // Opened так и не пришёл
	if len(r.fwd) != 1 || string(r.fwd[0]) != string(play) {
		t.Fatalf("после таймаута play должен вернуться mediad, возвращено %d команд", len(r.fwd))
	}
	if r.m.Handle(nil, pauseCmd("p1", "t1")) {
		t.Fatal("плеер, отданный mediad, больше не перехватывается")
	}
}

func TestNoTimeoutAfterPlaybackStarted(t *testing.T) {
	r := newRig(t)
	r.m.Handle("ctx", playCmd("p1", "t1", 0))
	r.m.Opened("p1", 200_000)
	r.advance(40 * time.Second)
	if len(r.fwd) != 0 {
		t.Fatal("играющий плеер откатываться не должен")
	}
}

func countCalls(b *fakeBackend, call string) (n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c == call {
			n++
		}
	}
	return
}
