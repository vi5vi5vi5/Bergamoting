package obs

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

var (
	lo1  = [4]byte{127, 0, 0, 1}
	shim = [4]byte{127, 0, 0, 2}
	t0   = time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
)

// seg — запись pcap (Ethernet + IPv4 + TCP) с меткой времени t0+at.
func seg(at time.Duration, src, dst [4]byte, sport, dport uint16, seq uint32, flags byte, payload []byte) []byte {
	pkt := make([]byte, 14+20+20, 14+20+20+len(payload))
	binary.BigEndian.PutUint16(pkt[12:], 0x0800)
	ip := pkt[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+20+len(payload)))
	ip[9] = 6
	copy(ip[12:], src[:])
	copy(ip[16:], dst[:])
	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[0:], sport)
	binary.BigEndian.PutUint16(tcp[2:], dport)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	tcp[12] = 5 << 4
	tcp[13] = flags
	pkt = append(pkt, payload...)

	ts := t0.Add(at)
	rec := make([]byte, 16)
	binary.LittleEndian.PutUint32(rec[0:], uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(rec[4:], uint32(ts.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(pkt)))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(pkt)))
	return append(rec, pkt...)
}

func pcapHeader() []byte {
	h := make([]byte, 24)
	binary.LittleEndian.PutUint32(h[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 65535)
	binary.LittleEndian.PutUint32(h[20:], 1)
	return h
}

func busFrame(typ wire.Type, n uint64) []byte {
	return wire.AppendFrame(nil, wire.Wrap(typ, pb.AppendUint(nil, 1, n)))
}

// session — одно соединение клиента через шим, как его видит tcpdump на lo: клиентская сторона после REDIRECT
// (порт 19906 на пути к шиму, порт 9906 в обратную сторону), сервисная — с 127.0.0.2.
func session(cap []byte, at time.Duration, cport, sport uint16, up, down, downB []byte) []byte {
	const syn, synack, ack = 0x02, 0x12, 0x10
	// рукопожатие обеих сторон
	cap = append(cap, seg(at, lo1, lo1, cport, 19906, 100, syn, nil)...)
	cap = append(cap, seg(at+50*time.Microsecond, shim, lo1, sport, 9906, 500, syn, nil)...)
	cap = append(cap, seg(at+60*time.Microsecond, lo1, shim, 9906, sport, 900, synack, nil)...)
	cap = append(cap, seg(at+70*time.Microsecond, lo1, lo1, 9906, cport, 700, synack, nil)...)
	// клиент → шим → сервис: шим добавляет 400 мкс
	cap = append(cap, seg(at+time.Millisecond, lo1, lo1, cport, 19906, 101, ack, up)...)
	cap = append(cap, seg(at+time.Millisecond+400*time.Microsecond, shim, lo1, sport, 9906, 501, ack, up)...)
	// сервис → шим → клиент: 300 мкс (downB — что получил шим, down — что он отдал клиенту)
	cap = append(cap, seg(at+2*time.Millisecond, lo1, shim, 9906, sport, 901, ack, downB)...)
	cap = append(cap, seg(at+2*time.Millisecond+300*time.Microsecond, lo1, lo1, 9906, cport, 701, ack, down)...)
	// соединение живёт заметно дольше «коротких» (их шим закрывает сам при недоступном сервисе)
	cap = append(cap, seg(at+200*time.Millisecond, lo1, lo1, cport, 19906, 101+uint32(len(up)), ack, nil)...)
	return cap
}

func runCheck(t *testing.T, cap []byte) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := ShimCheck{ClientPort: 19906, ServicePort: 9906, ShimAddr: net.ParseIP("127.0.0.2")}.Check(bytes.NewReader(cap), &out)
	return out.String(), err
}

func TestShimCheckClean(t *testing.T) {
	up := append(busFrame(wire.TypeAudioCommand, 7), busFrame(wire.TypeAudioCommand, 8)...)
	down := busFrame(wire.TypeAudioEvent, 3)
	cap := session(pcapHeader(), 0, 40000, 50000, up, down, down)
	// клиент, которому шим отказал: подключился и через миллисекунду был оборван, у шима SYN без ответа
	cap = append(cap, seg(10*time.Millisecond, lo1, lo1, 40002, 19906, 100, 0x02, nil)...)
	cap = append(cap, seg(10*time.Millisecond+20*time.Microsecond, shim, lo1, 50002, 9906, 500, 0x02, nil)...)
	cap = append(cap, seg(11*time.Millisecond, lo1, lo1, 40002, 19906, 101, 0x10, busFrame(wire.TypeAudioCommand, 9))...)

	out, err := runCheck(t, cap)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"→сервис 2/2 кадров, →клиент 1/1 кадров",
		"сопоставлено пар 1; коротких соединений при недоступном сервисе (шим закрыл сам, пока mediad перезапускался): 1",
		"сравнено кадров: 3, расхождений по содержимому: 0",
		"→сервис первый байт кадра: n=2  мин 400µs",
		"→клиент первый байт кадра: n=1  мин 300µs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в отчёте нет %q:\n%s", want, out)
		}
	}
}

func TestShimCheckDetectsAlteredFrame(t *testing.T) {
	up := busFrame(wire.TypeAudioCommand, 7)
	cap := session(pcapHeader(), 0, 40000, 50000, up, busFrame(wire.TypeAudioEvent, 3), busFrame(wire.TypeAudioEvent, 4))
	out, err := runCheck(t, cap)
	if err == nil {
		t.Fatalf("искажённый кадр не замечен:\n%s", out)
	}
	if !strings.Contains(out, "РАСХОЖДЕНИЙ: 1") {
		t.Errorf("в отчёте нет расхождения:\n%s", out)
	}
}

// Сервисное соединение с данными, которого клиент не просил, — тоже нарушение.
func TestShimCheckDetectsInventedTraffic(t *testing.T) {
	cap := pcapHeader()
	cap = append(cap, seg(0, shim, lo1, 50000, 9906, 500, 0x02, nil)...)
	cap = append(cap, seg(time.Millisecond, shim, lo1, 50000, 9906, 501, 0x10, busFrame(wire.TypeAudioCommand, 1))...)
	out, err := runCheck(t, cap)
	if err == nil {
		t.Fatalf("лишний трафик не замечен:\n%s", out)
	}
}

// Один и тот же эфемерный порт клиента переиспользуется: соединения не должны слипнуться.
func TestShimCheckPortReuse(t *testing.T) {
	f1 := busFrame(wire.TypeAudioCommand, 1)
	f2 := busFrame(wire.TypeAudioCommand, 2)
	ev := busFrame(wire.TypeAudioEvent, 5)
	cap := session(pcapHeader(), 0, 40000, 50000, f1, ev, ev)
	cap = session(cap, time.Second, 40000, 50000, f2, ev, ev)
	out, err := runCheck(t, cap)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "сопоставлено пар 2;") {
		t.Errorf("ждали 2 пары:\n%s", out)
	}
}
