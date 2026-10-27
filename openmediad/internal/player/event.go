// Package player — модель плеера mediad для музыкальных потоков: как из команды platformd получается событие
// AudioClientEvent (тип 127), и какими бывают переходы состояний. Это основа фазы 3 (шим сам отвечает за музыку)
// и фазы 5 (полная замена mediad). Всё выведено из записей живого трафика и сверено с ними побайтно (event_test.go);
// схем в прошивке нет, поэтому правила — наблюдение, а не спецификация.
package player

import (
	"sort"
	"strings"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
)

// Постоянные значения, которые mediad кладёт во все события музыкального плеера.
const (
	gainBits  = 0x3ff0000000000000 // 12: double 1.0 (громкость плеера)
	targetLUF = 0xc02c000000000000 // 21.3: double −14.0
)

// Meta — то, что события плеера наследуют от команды play (и предшествовавшей ей prefetch).
type Meta struct {
	Player quasar.Player
	Desc   []byte // поле 1 команды play: дескриптор трека
	Ref    string // поле 40 команды: идёт в поле 8 события
	Reused bool   // mediad запустил уже готовый плеер: после prefetch или возобновления с паузы (кэш плееров)
}

// MetaFrom собирает Meta из команды play. reused — запускается готовый плеер (после prefetch или с паузы).
func MetaFrom(play quasar.AudioCommand, reused bool) Meta {
	m := Meta{Player: play.Player(), Reused: reused}
	m.Desc, _ = pb.Bytes(play, 1)
	m.Ref, _ = pb.Str(play, 40)
	return m
}

// Snap — моментальный снимок плеера: всё, что меняется от события к событию.
type Snap struct {
	Kind  quasar.EventKind
	State quasar.PlayState

	PosMs  uint64 // 3.28
	Idx    uint64 // 3.12: целые секунды позиции (после перемотки отстаёт до первого прогресса)
	DurS   uint64 // 3.6: длительность, с
	HasDur bool   // поле 3.6 есть (до готовности конвейера его нет; значение 0 тоже бывает)
	Mono   uint64 // 3.29: монотонное время, мс; 0 — поля нет
	UUID   string // 3.22: идентификатор события
	Vsid   string // сессия просмотра: дописывается к URL и лежит в diag

	Start uint64 // 5: время запуска плеера, epoch мс
	Stop  uint64 // 7: время паузы/остановки, epoch мс; 0 — играет

	HasT9, T9   bool // 9: только у progress (false у первого после старта/перемотки)
	HasT17, T17 bool // 17: у flag-событий
	Gain        uint64

	// Over — поля дескриптора, которые берутся не из команды: mediad подставляет сюда то, что знает о треке сам
	// (URL и зеркала от ранее открытого плеера этого трека, поля 35/46 после команды crossfade). Поля 2 и 42 задаются
	// без «?vsid=», суффикс дописывается при сборке.
	Over []pb.Field

	Diag []byte // записи поля 14 одним куском
}

// Event собирает AudioClientEvent (тело сообщения типа 127).
func (m Meta) Event(s Snap) []byte {
	var b []byte
	b = pb.AppendUint(b, 1, uint64(s.Kind))
	b = pb.AppendUint(b, 2, uint64(s.State))
	b = pb.AppendBytes(b, 3, m.eventDesc(s))
	b = pb.AppendBytes(b, 4, playerBytes(m.Player))
	b = pb.AppendUint(b, 5, s.Start)
	b = pb.AppendUint(b, 7, s.Stop)
	b = pb.AppendString(b, 8, m.Ref)
	if s.HasT9 {
		b = pb.AppendUint(b, 9, b2u(s.T9))
	}
	b = pb.AppendBytes(b, 10, config10)
	gain := s.Gain
	if gain == 0 {
		gain = gainBits
	}
	b = pb.AppendFixed64(b, 12, gain)
	b = append(b, s.Diag...)
	b = pb.AppendBytes(b, 15, pb.AppendString(nil, 2, ""))
	if s.HasT17 {
		b = pb.AppendUint(b, 17, b2u(s.T17))
	}
	return b
}

func b2u(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

// config10 — поле 10 события, одинаковое во всех записях: {1:{1:f64(0x40c38d..) 2:f64(0x40d383..) 3:0 4:1} 2:0}.
var config10 = func() []byte {
	in := pb.AppendFixed64(nil, 1, 0x40c38d0000000000)
	in = pb.AppendFixed64(in, 2, 0x40d3830000000000)
	in = pb.AppendFixed64(in, 3, 0)
	in = pb.AppendUint(in, 4, 1)
	return pb.AppendUint(pb.AppendBytes(nil, 1, in), 2, 0)
}()

func playerBytes(p quasar.Player) []byte {
	b := pb.AppendUint(nil, 1, uint64(p.Kind))
	b = pb.AppendString(b, 2, p.ID)
	b = pb.AppendString(b, 3, p.Stream)
	return pb.AppendUint(b, 4, 0)
}

// eventDesc превращает дескриптор из команды в дескриптор события (правила выведены статистикой по записям).
func (m Meta) eventDesc(s Snap) []byte {
	suffix := "?vsid=" + s.Vsid
	over := map[uint64]bool{}
	for _, f := range s.Over {
		over[f.Num] = true
	}
	var fs []pb.Field
	r := pb.NewReader(m.Desc)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		if over[f.Num] {
			continue
		}
		switch f.Num {
		case 18:
			continue
		case 21:
			f.B = pb.AppendFixed64(append([]byte(nil), f.B...), 3, targetLUF)
		case 26:
			if m.Reused {
				continue
			}
		case 27:
			if !m.Reused {
				continue
			}
		}
		fs = append(fs, f)
	}
	for _, f := range s.Over {
		fs = append(fs, f)
	}
	for i := range fs {
		if fs[i].Num == 2 || fs[i].Num == 42 {
			fs[i].B = append(append([]byte(nil), fs[i].B...), suffix...)
		}
	}
	if s.HasDur {
		fs = append(fs, pb.Field{Num: 6, Wire: pb.WireVarint, U: s.DurS})
	}
	fs = append(fs,
		pb.Field{Num: 12, Wire: pb.WireVarint, U: s.Idx},
		pb.Field{Num: 22, Wire: pb.WireBytes, B: []byte(s.UUID)},
		pb.Field{Num: 28, Wire: pb.WireVarint, U: s.PosMs})
	if s.Mono > 0 {
		fs = append(fs, pb.Field{Num: 29, Wire: pb.WireVarint, U: s.Mono})
	}
	if m.Reused {
		fs = append(fs, pb.Field{Num: 30, Wire: pb.WireBytes})
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Num < fs[j].Num })
	var out []byte
	for _, f := range fs {
		out = pb.AppendField(out, f)
	}
	return out
}

// VsidOf достаёт vsid из URL события (то, что mediad дописал после «?vsid=»).
func VsidOf(url string) string {
	if i := strings.Index(url, "?vsid="); i >= 0 {
		return url[i+len("?vsid="):]
	}
	return ""
}
