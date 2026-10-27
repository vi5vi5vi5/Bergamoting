package player

import (
	"sort"
	"sync"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

// --- стенд: виртуальное время и поддельный бэкенд ---

type timer struct {
	at   time.Duration
	f    func()
	dead bool
}

type rig struct {
	t      *testing.T
	m      *Machine
	now    time.Duration
	timers []*timer
	mu     sync.Mutex
	evs    []got
	fwd    [][]byte
	be     *fakeBackend
}

type got struct {
	at   time.Duration
	body []byte
}

func (g got) kind() quasar.EventKind  { return quasar.AudioEvent(g.body).Kind() }
func (g got) state() quasar.PlayState { return quasar.AudioEvent(g.body).State() }
func (g got) player() string          { return quasar.AudioEvent(g.body).Player().ID }
func (g got) pos() uint64             { p, _ := quasar.AudioEvent(g.body).PositionMs(); return p }

type fakeBackend struct {
	mu     sync.Mutex
	r      *rig
	calls  []string
	pos    map[string]uint64
	since  map[string]time.Duration // момент старта вывода; нет ключа — не играет
	closed map[string]bool
	gain   float64
}

func (b *fakeBackend) rec(s string) { b.mu.Lock(); b.calls = append(b.calls, s); b.mu.Unlock() }

func (b *fakeBackend) Open(id string, t Track, off uint64) {
	b.rec("open " + id + " " + t.ID)
	b.mu.Lock()
	b.pos[id] = off
	b.mu.Unlock()
}

func (b *fakeBackend) Adopt(from, to string, off uint64) {
	b.rec("adopt " + from + "→" + to)
	b.mu.Lock()
	b.pos[to] = off
	b.mu.Unlock()
}

func (b *fakeBackend) Start(id string) {
	b.rec("start " + id)
	b.mu.Lock()
	b.since[id] = b.r.now
	b.mu.Unlock()
}

func (b *fakeBackend) Pause(id string) {
	b.rec("pause " + id)
	b.mu.Lock()
	b.pos[id] = b.posLocked(id)
	delete(b.since, id)
	b.mu.Unlock()
}

func (b *fakeBackend) Close(id string) {
	b.mu.Lock()
	b.calls = append(b.calls, "close "+id)
	b.closed[id] = true
	b.mu.Unlock()
}

func (b *fakeBackend) Seek(id string, p uint64) {
	b.rec("seek " + id)
	b.mu.Lock()
	b.pos[id] = p
	if _, ok := b.since[id]; ok {
		b.since[id] = b.r.now
	}
	b.mu.Unlock()
}

func (b *fakeBackend) Duck(g float64)              { b.mu.Lock(); b.gain = g; b.mu.Unlock() }
func (b *fakeBackend) Net(string) (uint64, uint64) { return 1000, 5000 }
func (b *fakeBackend) Pos(id string) uint64        { b.mu.Lock(); defer b.mu.Unlock(); return b.posLocked(id) }

func (b *fakeBackend) posLocked(id string) uint64 {
	p := b.pos[id]
	if s, ok := b.since[id]; ok {
		p += uint64((b.r.now - s) / time.Millisecond)
	}
	return p
}

func (b *fakeBackend) has(call string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c == call {
			return true
		}
	}
	return false
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t}
	r.be = &fakeBackend{r: r, pos: map[string]uint64{}, since: map[string]time.Duration{}, closed: map[string]bool{}, gain: 1}
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	r.m = &Machine{
		Emit: func(b []byte) {
			r.mu.Lock()
			r.evs = append(r.evs, got{r.now, append([]byte(nil), b...)})
			r.mu.Unlock()
		},
		Forward: func(_ any, b []byte) { r.fwd = append(r.fwd, append([]byte(nil), b...)) },
		Now:     func() time.Time { return base.Add(r.now) },
		Mono:    func() uint64 { return 1000000 + uint64(r.now/time.Millisecond) },
		After: func(d time.Duration, f func()) func() {
			tm := &timer{at: r.now + d, f: f}
			r.mu.Lock()
			r.timers = append(r.timers, tm)
			r.mu.Unlock()
			return func() { tm.dead = true }
		},
		Back: r.be,
		Logf: func(f string, a ...any) { t.Logf(f, a...) },
	}
	return r
}

// advance двигает виртуальное время, запуская таймеры по порядку.
func (r *rig) advance(d time.Duration) {
	end := r.now + d
	for {
		r.mu.Lock()
		sort.SliceStable(r.timers, func(i, j int) bool { return r.timers[i].at < r.timers[j].at })
		var next *timer
		for _, tm := range r.timers {
			if !tm.dead && tm.at <= end {
				next = tm
				break
			}
		}
		if next == nil {
			r.mu.Unlock()
			r.now = end
			return
		}
		next.dead = true
		if next.at > r.now {
			r.now = next.at
		}
		r.mu.Unlock()
		next.f()
	}
}

func (r *rig) events() []got {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]got(nil), r.evs...)
}

// states — только смены состояния (kind 1) в виде «ENDED/p1».
func (r *rig) states() (out []string) {
	for _, g := range r.events() {
		if g.kind() == 1 {
			out = append(out, g.state().String()+"/"+g.player())
		}
	}
	return
}

// --- команды ---

func cmdDesc(trackID, url string, offset uint64) []byte {
	var d []byte
	d = pb.AppendString(d, 1, trackID)
	d = pb.AppendString(d, 2, url)
	if offset > 0 {
		d = pb.AppendUint(d, 3, offset)
	}
	d = pb.AppendUint(d, 4, 1)
	d = pb.AppendString(d, 7, "{}")
	d = pb.AppendUint(d, 18, 1)
	d = pb.AppendString(d, 26, "")
	d = pb.AppendString(d, 39, "00112233445566778899aabbccddeeff")
	d = pb.AppendString(d, 42, url+"-alt1")
	d = pb.AppendString(d, 42, url+"-alt2")
	return d
}

func playerOf(id, track string) []byte {
	b := pb.AppendUint(nil, 1, 1)
	b = pb.AppendString(b, 2, id)
	b = pb.AppendString(b, 3, track)
	return pb.AppendUint(b, 4, 0)
}

func playCmdReason(id, track string, offset uint64, reason string) quasar.AudioCommand {
	var b []byte
	b = pb.AppendBytes(b, 1, cmdDesc(track, "https://s/"+track, offset))
	b = pb.AppendBytes(b, 22, playerOf(id, track))
	b = pb.AppendString(b, 40, "1223072313")
	return pb.AppendString(b, 52, reason)
}

func playCmd(id, track string, offset uint64) quasar.AudioCommand {
	return playCmdReason(id, track, offset, "AUDIO_PLAY directive")
}

func prefetchCmd(id, track string) quasar.AudioCommand {
	return playCmdReason(id, track, 0, "prefetchDirective: for prefetch")
}

func pauseCmd(id, track string) quasar.AudioCommand {
	b := pb.AppendString(nil, 4, "")
	b = pb.AppendBytes(b, 22, playerOf(id, track))
	return pb.AppendString(b, 52, "completeDirective: PAUSE branch")
}

func focusCmd(on uint64) quasar.AudioCommand { return pb.AppendUint(nil, 39, on) }
