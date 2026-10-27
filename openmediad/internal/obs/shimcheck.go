package obs

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sort"
	"time"
)

// ShimCheck — параметры проверки шима по записи tcpdump с обеими сторонами шима.
type ShimCheck struct {
	ClientPort  uint16 // порт, на котором шим слушает клиентов (19906)
	ServicePort uint16 // порт настоящего сервиса (9906)
	ShimAddr    net.IP // адрес, с которого шим ходит к сервису (127.0.0.2)
}

type frameRec struct {
	sum         [32]byte
	size        int
	first, done time.Time // пакет с первым байтом кадра и пакет, завершивший кадр
}

type stream struct {
	f      flow
	start  time.Time
	frames []frameRec
}

func (s *stream) add(seg tcpSeg, ts time.Time) {
	if len(seg.payload) > 0 && len(s.f.buf) == 0 {
		s.start = ts
	}
	s.f.add(seg.seq, seg.flags, seg.payload, func(frame []byte) {
		s.frames = append(s.frames, frameRec{sha256.Sum256(frame), len(frame), s.start, ts})
		s.start = ts // начало следующего кадра — не раньше этого пакета
	})
}

type peer struct {
	ip   [4]byte
	port uint16
}

type legConn struct {
	peer     peer      // клиент (сторона A) или локальный адрес шима (сторона B)
	syn      time.Time // SYN клиента этой стороны
	last     time.Time // последний пакет соединения
	up, down stream    // up — от клиента к сервису, down — обратно
}

func (lc *legConn) empty() bool { return len(lc.up.frames)+len(lc.down.frames) == 0 }

// legs — соединения одной стороны шима. Эфемерный порт может повториться, поэтому новый SYN
// на том же адресе начинает новое соединение, а прежнее откладывается в done.
type legs struct {
	live map[peer]*legConn
	done []*legConn
}

func newLegs() *legs { return &legs{live: map[peer]*legConn{}} }

// open вызывается по SYN клиента.
func (l *legs) open(p peer, ts time.Time) *legConn {
	if old := l.live[p]; old != nil {
		l.done = append(l.done, old)
	}
	lc := &legConn{peer: p, syn: ts}
	l.live[p] = lc
	return lc
}

// all — соединения, чей SYN попал в запись (середину чужого соединения сравнить нельзя), по времени SYN.
func (l *legs) all() []*legConn {
	s := append([]*legConn(nil), l.done...)
	for _, lc := range l.live {
		s = append(s, lc)
	}
	sort.Slice(s, func(i, j int) bool { return s[i].syn.Before(s[j].syn) })
	return s
}

// Check читает запись и сравнивает то, что шим принял от клиентов, с тем, что он отдал сервису, и обратно.
// Сторона A — клиент↔шим (порт ClientPort), сторона B — шим↔сервис (ShimAddr→ServicePort). Прямые соединения
// клиентов с сервисом (до включения шима) игнорируются. Сравниваются только соединения, чей SYN попал в запись.
func (c ShimCheck) Check(r io.Reader, out io.Writer) error {
	var shim [4]byte
	copy(shim[:], c.ShimAddr.To4())
	a, b := newLegs(), newLegs()
	err := readPcap(r, func(ts time.Time, link uint32, p []byte) {
		seg, ok := parseTCP(link, p)
		if !ok {
			return
		}
		const syn, ack = 0x02, 0x10
		isSyn := seg.flags&syn != 0 && seg.flags&ack == 0
		k := seg.key
		var lc *legConn
		var up bool
		switch {
		case k.dstPort == c.ClientPort: // клиент → шим (порт уже заменён правилом REDIRECT)
			pr := peer{k.src, k.srcPort}
			if isSyn {
				a.open(pr, ts)
			}
			lc, up = a.live[pr], true
		case k.srcPort == c.ClientPort: // шим → клиент, если запись видит порт шима
			lc = a.live[peer{k.dst, k.dstPort}]
		case k.dstPort == c.ServicePort && k.src == shim: // шим → сервис
			pr := peer{k.src, k.srcPort}
			if isSyn {
				b.open(pr, ts)
			}
			lc, up = b.live[pr], true
		case k.srcPort == c.ServicePort && k.dst == shim: // сервис → шим
			lc = b.live[peer{k.dst, k.dstPort}]
		case k.srcPort == c.ServicePort: // шим → клиент после обратной замены порта NAT: виден порт сервиса
			lc = a.live[peer{k.dst, k.dstPort}]
		default:
			return
		}
		if lc == nil {
			return // соединение началось до записи или это прямой клиент сервиса
		}
		lc.last = ts
		if up {
			lc.up.add(seg, ts)
		} else {
			lc.down.add(seg, ts)
		}
	})
	if err != nil {
		return err
	}
	return c.report(a.all(), b.all(), out)
}

// prefixEqual сравнивает кадры двух потоков по короткой длине: запись обрывается в разный момент,
// поэтому более длинный поток вправе быть продолжением короткого. Возвращает число сравнённых и разных кадров.
func prefixEqual(x, y []frameRec) (n int, bad int) {
	n = min(len(x), len(y))
	for i := 0; i < n; i++ {
		if x[i].sum != y[i].sum || x[i].size != y[i].size {
			bad++
		}
	}
	return n, bad
}

// pairWindow — насколько позже SYN клиента шим успевает открыть соединение к сервису.
const pairWindow = 200 * time.Millisecond

// shortLived — соединение клиента короче этого срока шим закрыл сам: сервис отказал (mediad перезапускался),
// а клиенты в такие моменты пробуют переподключаться сотнями раз в секунду. Пары к таким не ищем.
const shortLived = 50 * time.Millisecond

type pairing struct {
	pairs  [][2]*legConn
	outage []*legConn // клиентские соединения, закрытые шимом из-за недоступного сервиса (с данными или без)
	extra  []*legConn // сервисные соединения с данными, которым не нашлось клиента
}

func lenDiff(x, y *legConn) int {
	d := func(a, b int) int {
		if a > b {
			return a - b
		}
		return b - a
	}
	return d(len(x.up.frames), len(y.up.frames)) + d(len(x.down.frames), len(y.down.frames))
}

// pair сопоставляет соединения сторон A и B. Для долгих клиентских соединений ищется соединение стороны B
// с SYN в окне pairWindow и кадрами, совпадающими по префиксу (берётся то, у которого длины потоков ближе: рассылки
// сервиса у разных клиентов одинаковы, различаются в основном команды клиентов). Короткие и пустые соединения
// не сопоставляются, а только считаются.
func pair(as, bs []*legConn) pairing {
	var r pairing
	used := make([]bool, len(bs))
	for _, x := range as {
		if x.last.Sub(x.syn) < shortLived {
			r.outage = append(r.outage, x)
			continue
		}
		if x.empty() {
			continue
		}
		best, bestDiff, anyData := -1, 0, -1
		for i, y := range bs {
			if used[i] || y.empty() || y.syn.Before(x.syn) || y.syn.Sub(x.syn) > pairWindow {
				continue
			}
			if anyData < 0 {
				anyData = i
			}
			_, bu := prefixEqual(x.up.frames, y.up.frames)
			_, bd := prefixEqual(x.down.frames, y.down.frames)
			if bu+bd != 0 {
				continue
			}
			if d := lenDiff(x, y); best < 0 || d < bestDiff {
				best, bestDiff = i, d
			}
		}
		switch {
		case best >= 0:
			used[best] = true
			r.pairs = append(r.pairs, [2]*legConn{x, bs[best]})
		case anyData >= 0: // кандидат с данными есть, но кадры не совпали: покажем как расхождение
			used[anyData] = true
			r.pairs = append(r.pairs, [2]*legConn{x, bs[anyData]})
		default:
			r.outage = append(r.outage, x)
		}
	}
	for i, y := range bs {
		if !used[i] && !y.empty() {
			r.extra = append(r.extra, y)
		}
	}
	return r
}

func (c ShimCheck) report(as, bs []*legConn, out io.Writer) error {
	pr := pair(as, bs)
	emptyA, emptyB := 0, 0
	for _, x := range as {
		if x.empty() {
			emptyA++
		}
	}
	for _, y := range bs {
		if y.empty() {
			emptyB++
		}
	}
	var (
		compared, mismatched int
		lat                  [4][]time.Duration // →сервис (начало, конец), →клиент (начало, конец)
	)
	name := func(p peer) string { return fmt.Sprintf("%s:%d", net.IP(p.ip[:]), p.port) }
	for _, p := range pr.pairs {
		x, y := p[0], p[1]
		nu, bu := prefixEqual(x.up.frames, y.up.frames)
		nd, bd := prefixEqual(x.down.frames, y.down.frames)
		compared += nu + nd
		mismatched += bu + bd
		verdict := "совпало"
		if bu+bd > 0 {
			verdict = fmt.Sprintf("РАСХОЖДЕНИЙ: %d", bu+bd)
		}
		fmt.Fprintf(out, "клиент %s ↔ шим %s: →сервис %d/%d кадров, →клиент %d/%d кадров (клиентская сторона/сервисная): %s\n",
			name(x.peer), name(y.peer), len(x.up.frames), len(y.up.frames), len(x.down.frames), len(y.down.frames), verdict)
		for i := 0; i < nu; i++ {
			lat[0] = append(lat[0], y.up.frames[i].first.Sub(x.up.frames[i].first))
			lat[1] = append(lat[1], y.up.frames[i].done.Sub(x.up.frames[i].done))
		}
		for i := 0; i < nd; i++ {
			lat[2] = append(lat[2], x.down.frames[i].first.Sub(y.down.frames[i].first))
			lat[3] = append(lat[3], x.down.frames[i].done.Sub(y.down.frames[i].done))
		}
	}
	for _, x := range pr.extra {
		fmt.Fprintf(out, "шим %s: кадры к сервису есть (%d/%d), а клиентской пары нет\n", name(x.peer), len(x.up.frames), len(x.down.frames))
	}
	fmt.Fprintf(out, "\nсоединений клиент↔шим: %d (пустых %d), шим↔сервис: %d (пустых %d)\n", len(as), emptyA, len(bs), emptyB)
	fmt.Fprintf(out, "сопоставлено пар %d; коротких соединений при недоступном сервисе (шим закрыл сам, пока mediad перезапускался): %d; сервисных с данными без пары: %d\n",
		len(pr.pairs), len(pr.outage), len(pr.extra))
	fmt.Fprintf(out, "сравнено кадров: %d, расхождений по содержимому: %d\n", compared, mismatched)
	fmt.Fprintln(out, "задержка, которую добавляет шим (метки пакетов tcpdump, одни часы):")
	for i, l := range lat {
		dir, what := "→сервис", "первый байт кадра"
		if i >= 2 {
			dir = "→клиент"
		}
		if i%2 == 1 {
			what = "кадр целиком    "
		}
		if len(l) == 0 {
			fmt.Fprintf(out, "  %s %s: нет данных\n", dir, what)
			continue
		}
		sort.Slice(l, func(p, q int) bool { return l[p] < l[q] })
		at := func(q float64) time.Duration { return l[min(len(l)-1, int(q*float64(len(l))))].Round(time.Microsecond) }
		fmt.Fprintf(out, "  %s %s: n=%d  мин %s  p50 %s  p90 %s  p99 %s  макс %s\n",
			dir, what, len(l), l[0].Round(time.Microsecond), at(0.5), at(0.9), at(0.99), l[len(l)-1].Round(time.Microsecond))
	}
	if mismatched > 0 || len(pr.extra) > 0 {
		return fmt.Errorf("шим: расхождений %d, сервисных соединений с данными без пары %d", mismatched, len(pr.extra))
	}
	return nil
}
