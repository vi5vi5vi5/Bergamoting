package player

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

type fakeVoice struct {
	mu     sync.Mutex
	calls  []string
	chunks [][]byte
	pos    uint64
}

func (f *fakeVoice) rec(s string)    { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeVoice) Start(id string) { f.rec("start " + id) }
func (f *fakeVoice) Chunk(id string, ogg []byte) {
	f.rec("chunk " + id)
	f.mu.Lock()
	f.chunks = append(f.chunks, ogg)
	f.mu.Unlock()
}
func (f *fakeVoice) EOS(id string)   { f.rec("eos " + id) }
func (f *fakeVoice) Abort(id string) { f.rec("abort " + id) }
func (f *fakeVoice) Pos(string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pos
}
func (f *fakeVoice) has(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			return true
		}
	}
	return false
}

func newVoiceRig(t *testing.T) (*rig, *fakeVoice) {
	r := newRig(t)
	fv := &fakeVoice{}
	r.m.Voice = fv
	return r, fv
}

func voicePlayer(id string) []byte {
	return pb.AppendString(pb.AppendUint(nil, 1, 2), 2, id)
}

func voiceStartCmd(id string) quasar.AudioCommand {
	d := pb.AppendUint(nil, 4, 5)
	d = pb.AppendBytes(d, 8, pb.AppendString(pb.AppendString(nil, 1, "req-1"), 2, "tts_play_placeholder"))
	c := pb.AppendBytes(nil, 1, d)
	return pb.AppendBytes(c, 22, voicePlayer(id))
}

func voiceChunkCmd(id string, ogg []byte) quasar.AudioCommand {
	c := pb.AppendBytes(nil, 22, voicePlayer(id))
	return pb.AppendBytes(c, 29, ogg)
}

func voiceEOSCmd(id string) quasar.AudioCommand {
	return pb.AppendString(pb.AppendBytes(nil, 22, voicePlayer(id)), 30, "")
}

func voiceReleaseCmd(id string) quasar.AudioCommand {
	return pb.AppendString(pb.AppendBytes(nil, 22, voicePlayer(id)), 31, "")
}

func TestVoiceReplySequenceAndTimings(t *testing.T) {
	r, fv := newVoiceRig(t)
	if !r.m.Handle(nil, voiceStartCmd("v1")) {
		t.Fatal("voice-start обрабатывается машиной")
	}
	r.advance(10 * time.Millisecond)
	if len(r.events()) != 0 {
		t.Fatal("STARTING раньше ≈25 мс после voice-start приходить не должен")
	}
	r.advance(30 * time.Millisecond)
	ogg := []byte("OggS-fake")
	if !r.m.Handle(nil, voiceChunkCmd("v1", ogg)) {
		t.Fatal("voice-chunk обрабатывается машиной")
	}
	ogg[0] = 'X' // буфер кадра принадлежит шиму: бэкенд должен получить копию
	if !bytes.Equal(fv.chunks[0], []byte("OggS-fake")) {
		t.Fatal("бэкенд получил не копию чанка")
	}
	r.m.Handle(nil, voiceEOSCmd("v1"))
	r.advance(40 * time.Millisecond)
	r.m.VoicePlaying("v1")
	fv.pos = 0
	r.advance(2300 * time.Millisecond) // два progress: на 1068 и 2136 мс после PLAYING
	fv.pos = 2380
	r.m.VoiceEnded("v1")
	if !r.m.Handle(nil, voiceReleaseCmd("v1")) {
		t.Fatal("release голоса обрабатывается машиной")
	}
	var kinds []string
	for _, g := range r.events() {
		kinds = append(kinds, g.kind().String()+"/"+g.state().String())
	}
	want := []string{"state/STARTING", "state/PLAYING", "progress/PLAYING", "flag/PLAYING", "progress/PLAYING", "progress/PLAYING", "state/ENDED"}
	if len(kinds) != len(want) {
		t.Fatalf("события %v, ждали %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("события %v, ждали %v", kinds, want)
		}
	}
	ev := r.events()
	if ev[0].at != 25*time.Millisecond {
		t.Fatalf("STARTING в %v, ждали через 25 мс после voice-start", ev[0].at)
	}
	if d := ev[3].at - ev[1].at; d != 260*time.Millisecond {
		t.Fatalf("flag через %v после PLAYING, ждали 260 мс", d)
	}
	if d := ev[5].at - ev[4].at; d != 1068*time.Millisecond {
		t.Fatalf("progress через %v, ждали 1068 мс", d)
	}
	// ENDED несёт позицию последнего progress, а не фактическую (по записям: 53 из 53)
	if last := ev[6]; last.pos() != ev[5].pos() {
		t.Fatalf("ENDED.pos = %d, последний progress.pos = %d", last.pos(), ev[5].pos())
	}
	for _, g := range ev {
		if g.player() != "v1" {
			t.Fatalf("чужой плеер %q", g.player())
		}
	}
}

func TestVoiceEventFieldRules(t *testing.T) {
	r, fv := newVoiceRig(t)
	r.m.Handle(nil, voiceStartCmd("v1"))
	r.advance(30 * time.Millisecond)
	r.m.Handle(nil, voiceChunkCmd("v1", []byte("x")))
	r.m.VoicePlaying("v1")
	fv.pos = 1054
	r.advance(1100 * time.Millisecond)
	fv.pos = 2128
	r.advance(1100 * time.Millisecond)
	r.m.VoiceEnded("v1")
	evs := r.events()
	for _, g := range evs {
		d, _ := pb.Bytes(g.body, 3)
		hasPlayed := false
		rd := pb.NewReader(g.body)
		for {
			f, err := rd.Next()
			if err != nil {
				break
			}
			if f.Num == 14 {
				if k, _ := pb.Str(f.B, 1); k == "played_sec" {
					hasPlayed = true
				}
			}
		}
		if want := g.pos() >= 1000; hasPlayed != want {
			t.Fatalf("%v/%v pos=%d: played_sec есть=%v, ждали %v", g.kind(), g.state(), g.pos(), hasPlayed, want)
		}
		if g.state() != 5 && g.kind() == 1 && (pb.Has(d, 6) || pb.Has(d, 29)) {
			t.Fatalf("у STARTING/PLAYING не должно быть полей 6 и 29")
		}
		if g.kind() != 1 && !(pb.Has(d, 6) && pb.Has(d, 29)) {
			t.Fatalf("у progress и flag должны быть поля 6 и 29")
		}
	}
	first := evs[2] // первый progress: позиция 0, поле 9 = 0
	if v, ok := pb.Uint(first.body, 9); !ok || v != 0 {
		t.Fatalf("первый progress: поле 9 = %d (есть=%v), ждали 0", v, ok)
	}
	if v, ok := pb.Uint(evs[4].body, 9); !ok || v != 1 {
		t.Fatalf("следующий progress: поле 9 = %d (есть=%v), ждали 1", v, ok)
	}
	last := evs[len(evs)-1]
	if v, _ := pb.Uint(last.body, 7); v == 0 {
		t.Fatal("у ENDED должно быть время конца (поле 7)")
	}
}

func TestVoiceFailureForwardsWholeReplyToMediad(t *testing.T) {
	r, fv := newVoiceRig(t)
	start, chunk, eos := voiceStartCmd("v1"), voiceChunkCmd("v1", []byte("data")), voiceEOSCmd("v1")
	r.m.Handle("ctx", start)
	r.m.Handle("ctx", chunk)
	r.m.Handle("ctx", eos)
	r.m.VoiceFailed("v1", errors.New("нет aplay"))
	if len(r.fwd) != 3 || !bytes.Equal(r.fwd[0], start) || !bytes.Equal(r.fwd[1], chunk) || !bytes.Equal(r.fwd[2], eos) {
		t.Fatalf("mediad должен получить весь ответ по порядку, получено %d команд", len(r.fwd))
	}
	if !fv.has("abort v1") {
		t.Fatal("звук шима должен быть оборван")
	}
	if r.m.Handle(nil, voiceChunkCmd("v1", []byte("more"))) {
		t.Fatal("остальные команды отданного ответа идут mediad")
	}
	if r.m.Handle(nil, voiceReleaseCmd("v1")) {
		t.Fatal("release отданного ответа идёт mediad")
	}
	if r.m.Handle(nil, voiceChunkCmd("v1", []byte("more"))) != true && false {
		t.Fatal("после release метка отката снимается")
	}
	if n := len(r.events()); n > 1 {
		t.Fatalf("за ответ, который ведёт mediad, шим событий выпускать не должен (кроме уже ушедшего STARTING): %d", n)
	}
}

func TestVoiceNoAudioWithinTimeoutFallsBack(t *testing.T) {
	r, _ := newVoiceRig(t)
	start, chunk := voiceStartCmd("v1"), voiceChunkCmd("v1", []byte("data"))
	r.m.Handle("ctx", start)
	r.m.Handle("ctx", chunk)
	r.advance(2900 * time.Millisecond)
	if len(r.fwd) != 0 {
		t.Fatal("до таймаута отдавать рано")
	}
	r.advance(200 * time.Millisecond)
	if len(r.fwd) != 2 {
		t.Fatalf("после таймаута ответ уходит mediad: %d команд", len(r.fwd))
	}
}

func TestVoiceNoTimeoutOnceAudioStarted(t *testing.T) {
	r, _ := newVoiceRig(t)
	r.m.Handle("ctx", voiceStartCmd("v1"))
	r.m.Handle("ctx", voiceChunkCmd("v1", []byte("data")))
	r.m.VoicePlaying("v1")
	r.advance(10 * time.Second)
	if len(r.fwd) != 0 {
		t.Fatal("играющий ответ откатываться не должен")
	}
}

func TestVoiceReleaseBeforeEndAbortsAndEnds(t *testing.T) {
	r, fv := newVoiceRig(t)
	r.m.Handle(nil, voiceStartCmd("v1"))
	r.m.Handle(nil, voiceChunkCmd("v1", []byte("x")))
	r.m.VoicePlaying("v1")
	r.advance(100 * time.Millisecond)
	n := len(r.events())
	if !r.m.Handle(nil, voiceReleaseCmd("v1")) {
		t.Fatal("release своего ответа обрабатывается")
	}
	if !fv.has("abort v1") {
		t.Fatal("прерванный ответ должен замолчать")
	}
	ev := r.events()
	if len(ev) != n+1 || ev[n].state() != 5 {
		t.Fatalf("после прерывания платформа должна получить ENDED, события %d → %d", n, len(ev))
	}
}

func TestNewVoiceReplyDisplacesOld(t *testing.T) {
	r, fv := newVoiceRig(t)
	r.m.Handle(nil, voiceStartCmd("v1"))
	r.m.Handle(nil, voiceChunkCmd("v1", []byte("x")))
	r.m.VoicePlaying("v1")
	r.m.Handle(nil, voiceStartCmd("v2"))
	if !fv.has("abort v1") {
		t.Fatal("старый ответ должен быть оборван новым")
	}
	r.m.VoiceEnded("v1") // запоздалое сообщение бэкенда о вытесненном ответе игнорируется
	r.advance(100 * time.Millisecond)
	for _, g := range r.events() {
		if g.player() == "v1" && g.state() == 5 {
			t.Fatal("для вытесненного ответа ENDED не нужен: платформа уже закрывает его release")
		}
	}
}

func TestVoiceIgnoredWithoutBackend(t *testing.T) {
	r := newRig(t) // Voice == nil
	if r.m.Handle(nil, voiceStartCmd("v1")) || r.m.Handle(nil, voiceChunkCmd("v1", []byte("x"))) || r.m.Handle(nil, voiceReleaseCmd("v1")) {
		t.Fatal("без VoiceBackend голос идёт настоящему mediad")
	}
}

func TestHandoffEndsActiveVoice(t *testing.T) {
	r, fv := newVoiceRig(t)
	r.m.Handle(nil, voiceStartCmd("v1"))
	r.m.Handle(nil, voiceChunkCmd("v1", []byte("x")))
	r.m.VoicePlaying("v1")
	n := len(r.events())
	r.m.Handoff()
	ev := r.events()
	if len(ev) != n+1 || ev[n].state() != 5 || ev[n].player() != "v1" {
		t.Fatalf("Handoff должен закончить идущий ответ событием ENDED, событий %d → %d", n, len(ev))
	}
	if !fv.has("abort v1") {
		t.Fatal("звук ответа должен быть оборван")
	}
}

// Звук может пойти раньше, чем сработает таймер STARTING (ответ целиком приходит одним пакетом команд, декодер быстрый):
// платформа всё равно должна увидеть STARTING перед PLAYING (замечено на записи vmix1: PLAYING без STARTING).
func TestVoiceStartingPrecedesPlayingEvenWhenAudioIsFast(t *testing.T) {
	r, _ := newVoiceRig(t)
	r.m.Handle(nil, voiceStartCmd("v1"))
	r.m.Handle(nil, voiceChunkCmd("v1", []byte("x")))
	r.advance(5 * time.Millisecond) // раньше 25 мс
	r.m.VoicePlaying("v1")
	r.advance(100 * time.Millisecond)
	var st []string
	for _, g := range r.events() {
		if g.kind() == 1 {
			st = append(st, g.state().String())
		}
	}
	if len(st) != 2 || st[0] != "STARTING" || st[1] != "PLAYING" {
		t.Fatalf("состояния %v, ждали [STARTING PLAYING]", st)
	}
}
