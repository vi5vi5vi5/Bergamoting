package player

import (
	"sort"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

// События «простых» плееров: голос Алисы (вид 2) и воспроизведение файла (вид 4: звонок таймера и будильника, системные
// звуки). Формат отличается от музыкального: нет сети и трека, дескриптор короткий, верхний уровень беднее (нет поля 10,
// поле 8 — пустая строка, поле 15 — пустое). Восстановлено по записям и проверено побайтно (plain_event_test.go).

// PlainMeta — то, что события простого плеера наследуют от стартовой команды.
type PlainMeta struct {
	Player    quasar.Player // вид и uuid (для голоса — {1:2, 2:uuid})
	PlayerRaw []byte        // поле 22 команды целиком: у плеера файлов в нём есть ещё поле 4 = 1; если задано, идёт в событие как есть
	Desc      []byte        // поле 1 команды: у голоса {4: 5, 8: {…}}, у файла {4: 4, 19: 0, 25: путь, 50: 0}
}

// PlainMetaFrom собирает PlainMeta из команды voice-start.
func PlainMetaFrom(start quasar.AudioCommand) PlainMeta {
	m := PlainMeta{Player: start.Player()}
	m.Desc, _ = pb.Bytes(start, 1)
	m.PlayerRaw, _ = pb.Bytes(start, 22)
	return m
}

// PlainSnap — моментальный снимок голосового плеера.
type PlainSnap struct {
	Kind  quasar.EventKind
	State quasar.PlayState

	PosMs uint64 // 3.28
	Idx   uint64 // 3.12: целые секунды позиции
	Has6  bool   // поле 3.6 есть (у голоса всегда 0; у файла — длительность в секундах, 0 пока неизвестна)
	Dur6  uint64 // значение 3.6
	Mono  uint64 // 3.29: CLOCK_MONOTONIC, мс; 0 — поля нет
	UUID  string // 3.22: новый на каждое событие

	Start, Stop uint64 // 5 и 7 (epoch мс); Stop = 0, пока играет
	HasT9, T9   bool   // 9: у progress
	HasT17, T17 bool   // 17: у flag

	Vsid, Mark string
	PlayedSec  uint64 // diag played_sec
	HasPlayed  bool
	RSS        uint64 // diag rss_mem
	Bitrate    uint64 // diag bitrate (у плеера файлов в flag и ENDED); 0 — нет
}

// Event собирает AudioClientEvent голосового плеера.
func (m PlainMeta) Event(s PlainSnap) []byte {
	var b []byte
	b = pb.AppendUint(b, 1, uint64(s.Kind))
	b = pb.AppendUint(b, 2, uint64(s.State))
	b = pb.AppendBytes(b, 3, m.eventDesc(s))
	if m.PlayerRaw != nil {
		b = pb.AppendBytes(b, 4, m.PlayerRaw)
	} else {
		pl := pb.AppendUint(nil, 1, uint64(m.Player.Kind))
		b = pb.AppendBytes(b, 4, pb.AppendString(pl, 2, m.Player.ID))
	}
	b = pb.AppendUint(b, 5, s.Start)
	b = pb.AppendUint(b, 7, s.Stop)
	b = pb.AppendString(b, 8, "")
	if s.HasT9 {
		b = pb.AppendUint(b, 9, b2u(s.T9))
	}
	b = pb.AppendFixed64(b, 12, gainBits)
	b = append(b, plainDiag(s)...)
	b = pb.AppendString(b, 15, "")
	if s.HasT17 {
		b = pb.AppendUint(b, 17, b2u(s.T17))
	}
	return b
}

func (m PlainMeta) eventDesc(s PlainSnap) []byte {
	var fs []pb.Field
	r := pb.NewReader(m.Desc)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		fs = append(fs, f)
	}
	if s.Has6 {
		fs = append(fs, pb.Field{Num: 6, Wire: pb.WireVarint, U: s.Dur6})
	}
	fs = append(fs,
		pb.Field{Num: 12, Wire: pb.WireVarint, U: s.Idx},
		pb.Field{Num: 22, Wire: pb.WireBytes, B: []byte(s.UUID)},
		pb.Field{Num: 28, Wire: pb.WireVarint, U: s.PosMs})
	if s.Mono > 0 {
		fs = append(fs, pb.Field{Num: 29, Wire: pb.WireVarint, U: s.Mono})
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Num < fs[j].Num })
	var out []byte
	for _, f := range fs {
		out = pb.AppendField(out, f)
	}
	return out
}

// plainDiag — поле 14 голоса: без сетевых счётчиков, у progress и ENDED добавляется played_sec.
func plainDiag(s PlainSnap) []byte {
	num := func(k string, v uint64) []byte {
		return pb.AppendBytes(nil, 14, pb.AppendUint(pb.AppendString(nil, 1, k), 3, v))
	}
	str := func(k, v string) []byte {
		return pb.AppendBytes(nil, 14, pb.AppendString(pb.AppendString(nil, 1, k), 2, v))
	}
	var b []byte
	b = append(b, str("mark", s.Mark)...)
	b = append(b, str("vsid", s.Vsid)...)
	b = append(b, num("buffering", 0)...)
	b = append(b, num("rss_mem", s.RSS)...)
	b = append(b, num("rss_lim", 48800)...)
	if s.Bitrate > 0 {
		b = append(b, num("bitrate", s.Bitrate)...)
	}
	if s.HasPlayed {
		b = append(b, num("played_sec", s.PlayedSec)...)
	}
	return b
}
