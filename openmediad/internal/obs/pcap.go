package obs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
	"openmediad/internal/wire"
)

// DecodePcap читает classic-pcap (tcpdump -w) с трафиком шины на lo и печатает кадры обоих направлений.
// Загрузка в память кадр за кадром: файл целиком не читается, поэтому подходит и для `tcpdump -w - | omd pcap -`.
func DecodePcap(r io.Reader, o Opt, out io.Writer) error {
	d := &decoder{o: o, out: out, flows: map[flowKey]*flow{}}
	return readPcap(r, func(ts time.Time, link uint32, p []byte) {
		d.ts = ts
		d.packet(link, p)
	})
}

// readPcap читает classic-pcap пакет за пакетом и вызывает fn; срез данных действителен только внутри вызова.
func readPcap(r io.Reader, fn func(ts time.Time, link uint32, p []byte)) error {
	br := bufio.NewReaderSize(r, 256<<10)
	var hdr [24]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("pcap: заголовок: %w", err)
	}
	var bo binary.ByteOrder
	var nano bool
	switch binary.LittleEndian.Uint32(hdr[:4]) {
	case 0xa1b2c3d4:
		bo = binary.LittleEndian
	case 0xa1b23c4d:
		bo, nano = binary.LittleEndian, true
	default:
		switch binary.BigEndian.Uint32(hdr[:4]) {
		case 0xa1b2c3d4:
			bo = binary.BigEndian
		case 0xa1b23c4d:
			bo, nano = binary.BigEndian, true
		default:
			return errors.New("pcap: неизвестный формат (нужен classic pcap, не pcapng)")
		}
	}
	link := bo.Uint32(hdr[20:24])

	var rec [16]byte
	var data []byte
	for {
		if _, err := io.ReadFull(br, rec[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		incl := bo.Uint32(rec[8:12])
		if incl > 1<<20 {
			return fmt.Errorf("pcap: пакет %d Б", incl)
		}
		if uint32(cap(data)) < incl {
			data = make([]byte, incl)
		}
		data = data[:incl]
		if _, err := io.ReadFull(br, data); err != nil {
			return nil
		}
		frac := int64(bo.Uint32(rec[4:8]))
		if !nano {
			frac *= 1000
		}
		fn(time.Unix(int64(bo.Uint32(rec[0:4])), frac).UTC(), link, data)
	}
}

type flowKey struct {
	src, dst         [4]byte
	srcPort, dstPort uint16
}

type flow struct {
	next   uint32
	synced bool
	have   bool
	buf    []byte
}

type decoder struct {
	o     Opt
	out   io.Writer
	ts    time.Time
	flows map[flowKey]*flow
	walk  func(Frame) // если задано, кадры отдаются сюда вместо печати
}

// Frame — один кадр шины из записи (для разбора программой, а не глазами).
type Frame struct {
	Time     time.Time
	Svc      string  // имя сервиса по порту
	ToSvc    bool    // true — клиент → сервис, false — сервис → клиент
	Client   uint16  // порт клиентского сокета: различает соединения одного сервиса
	ClientIP [4]byte // адрес клиентской стороны: у соединений шима к настоящему сервису это 127.0.0.2
	Data     []byte  // кадр без длины; действителен только внутри вызова fn
}

// WalkPcap собирает кадры из записи tcpdump так же, как DecodePcap, но отдаёт их fn по порядку записи.
func WalkPcap(r io.Reader, fn func(Frame)) error {
	d := &decoder{flows: map[flowKey]*flow{}, walk: fn}
	return readPcap(r, func(ts time.Time, link uint32, p []byte) {
		d.ts = ts
		d.packet(link, p)
	})
}

// tcpSeg — один TCP-сегмент IPv4 из записи.
type tcpSeg struct {
	key     flowKey
	seq     uint32
	flags   byte
	payload []byte
}

// parseTCP достаёт TCP-сегмент из пакета канального уровня; false — это не IPv4/TCP.
func parseTCP(link uint32, p []byte) (tcpSeg, bool) {
	var off int
	var proto uint16
	switch link {
	case 1: // Ethernet
		if len(p) < 14 {
			return tcpSeg{}, false
		}
		proto, off = binary.BigEndian.Uint16(p[12:14]), 14
	case 113: // Linux cooked
		if len(p) < 16 {
			return tcpSeg{}, false
		}
		proto, off = binary.BigEndian.Uint16(p[14:16]), 16
	case 276: // Linux cooked v2
		if len(p) < 20 {
			return tcpSeg{}, false
		}
		proto, off = binary.BigEndian.Uint16(p[0:2]), 20
	default:
		return tcpSeg{}, false
	}
	if proto != 0x0800 || len(p) < off+20 {
		return tcpSeg{}, false
	}
	ip := p[off:]
	ihl := int(ip[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	if ip[9] != 6 || ihl < 20 || total > len(ip) || total < ihl+20 {
		return tcpSeg{}, false
	}
	tcp := ip[ihl:total]
	doff := int(tcp[12]>>4) * 4
	if doff < 20 || doff > len(tcp) {
		return tcpSeg{}, false
	}
	s := tcpSeg{
		seq:     binary.BigEndian.Uint32(tcp[4:8]),
		flags:   tcp[13],
		payload: tcp[doff:],
	}
	s.key.srcPort = binary.BigEndian.Uint16(tcp[0:2])
	s.key.dstPort = binary.BigEndian.Uint16(tcp[2:4])
	copy(s.key.src[:], ip[12:16])
	copy(s.key.dst[:], ip[16:20])
	return s, true
}

func (d *decoder) packet(link uint32, p []byte) {
	seg, ok := parseTCP(link, p)
	if !ok {
		return
	}
	k := seg.key
	svc, dir := "", ""
	var client uint16
	var clientIP [4]byte
	if s, ok := Ports[k.srcPort]; ok {
		svc, dir, client, clientIP = s, "S→C", k.dstPort, k.dst
	} else if s, ok := Ports[k.dstPort]; ok {
		svc, dir, client, clientIP = s, "C→S", k.srcPort, k.src
	} else {
		return
	}
	f := d.flows[k]
	if f == nil {
		f = &flow{}
		d.flows[k] = f
	}
	f.add(seg.seq, seg.flags, seg.payload, func(frame []byte) {
		if d.walk != nil {
			d.walk(Frame{Time: d.ts, Svc: svc, ToSvc: dir == "C→S", Client: client, ClientIP: clientIP, Data: frame})
			return
		}
		d.frame(svc, dir, frame)
	})
}

// add собирает поток байт одного направления. Loopback идёт по порядку, поэтому достаточно отслеживать
// ожидаемый seq; при дыре поток пересинхронизируется по границе кадра.
func (f *flow) add(seq uint32, flags byte, payload []byte, emit func([]byte)) {
	const syn, rst, fin = 0x02, 0x04, 0x01
	if flags&syn != 0 {
		f.next, f.have, f.synced, f.buf = seq+1, true, true, f.buf[:0]
		return
	}
	if flags&(rst|fin) != 0 && len(payload) == 0 {
		f.have = false
		return
	}
	if len(payload) == 0 {
		return
	}
	if !f.have {
		f.next, f.have, f.synced = seq, true, false
	}
	if seq != f.next {
		delta := int32(seq - f.next)
		switch {
		case delta < 0 && int(-delta) < len(payload): // частичный повтор: берём только новый хвост
			payload = payload[-delta:]
			seq = f.next
		case delta < 0: // полный повтор
			return
		default: // потеря: начинаем заново
			f.buf, f.synced = f.buf[:0], false
		}
	}
	f.next = seq + uint32(len(payload))
	if !f.synced {
		if len(payload) < 4 {
			return
		}
		n := binary.LittleEndian.Uint32(payload[:4])
		if n == 0 || n > wire.MaxFrame {
			return
		}
		if int(n) <= len(payload)-4 && pb.Validate(payload[4:4+n]) != nil {
			return
		}
		f.synced = true
	}
	f.buf = append(f.buf, payload...)
	for len(f.buf) >= 4 {
		n := binary.LittleEndian.Uint32(f.buf[:4])
		if n == 0 || n > wire.MaxFrame {
			f.buf, f.synced = f.buf[:0], false
			return
		}
		if len(f.buf) < 4+int(n) {
			return
		}
		emit(f.buf[4 : 4+n])
		f.buf = f.buf[4+n:]
	}
	if cap(f.buf) > 1<<20 && len(f.buf) == 0 {
		f.buf = nil
	}
}

func (d *decoder) frame(svc, dir string, frame []byte) {
	m, err := wire.Unwrap(frame)
	if err != nil {
		return
	}
	if d.o.VoiceDir != "" && m.Type == wire.TypeAudioCommand {
		c := quasar.AudioCommand(m.Body)
		if chunk, ok := c.VoiceChunk(); ok {
			name := fmt.Sprintf("%s-%s.ogg", d.ts.Format("150405.000"), short(c.Player().ID))
			if err := os.WriteFile(filepath.Join(d.o.VoiceDir, name), chunk, 0o600); err != nil {
				fmt.Fprintf(d.out, "%s voice-dir: %v\n", d.ts.Format("15:04:05.000"), err)
			}
		}
	}
	if line, show := Describe(svc, m, d.o); show {
		fmt.Fprintf(d.out, "%s %-11s %s %s\n", d.ts.Format("15:04:05.000"), svc, dir, line)
		if d.o.Dump && d.dumpType(m.Type) {
			max := d.o.DumpMax
			if max <= 0 {
				max = 160
			}
			fmt.Fprintf(d.out, "    %s\n", pb.Dump(m.Body, max))
		}
	}
}

// dumpType — печатать ли сообщение этого типа деревом полей (-dump).
func (d *decoder) dumpType(t wire.Type) bool {
	if len(d.o.DumpTypes) > 0 {
		return d.o.DumpTypes[t]
	}
	return t == wire.TypeAudioCommand || t == wire.TypeAudioEvent
}
