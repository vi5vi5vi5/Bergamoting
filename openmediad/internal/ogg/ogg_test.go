package ogg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// page собирает одну страницу Ogg (контрольная сумма нулевая: разборщик её не проверяет).
func page(flags byte, granule int64, segs []byte, body []byte) []byte {
	h := make([]byte, headerLen, headerLen+len(segs)+len(body))
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], uint64(granule))
	h[26] = byte(len(segs))
	h = append(h, segs...)
	return append(h, body...)
}

// lacing раскладывает пакет по сегментам: 255 значит «продолжается», пакет кратной 255 длины кончается нулём.
func lacing(n int) []byte {
	var s []byte
	for ; n >= 255; n -= 255 {
		s = append(s, 255)
	}
	return append(s, byte(n))
}

type got struct {
	data      []byte
	granule   int64
	last, eos bool
}

func run(t *testing.T, stream []byte, chunk int) []got {
	t.Helper()
	var d Demuxer
	var out []got
	emit := func(p Packet) error {
		out = append(out, got{append([]byte(nil), p.Data...), p.Granule, p.LastOnPage, p.EOS})
		return nil
	}
	for i := 0; i < len(stream); i += chunk {
		if err := d.Write(stream[i:min(i+chunk, len(stream))], emit); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7)
	}
	return b
}

func TestPacketsAcrossPagesAndChunks(t *testing.T) {
	a, b, c, e := pattern(10, 1), pattern(300, 2), pattern(510, 3), pattern(0, 0) // 510 = 2×255: нужен нулевой сегмент
	// страница 1: пакет a целиком и начало b (сегмент 255, остальное на следующей странице)
	p1 := page(flagBOS, -1, append(lacing(len(a)), 255), append(append([]byte(nil), a...), b[:255]...))
	// страница 2: конец b, пакет c (255+255+0), пустой пакет e
	segs2 := append(append(lacing(len(b)-255), lacing(len(c))...), lacing(0)...)
	body2 := append(append(append([]byte(nil), b[255:]...), c...), e...)
	p2 := page(flagContinued|flagEOS, 4800, segs2, body2)
	stream := append(p1, p2...)

	want := [][]byte{a, b, c, e}
	for _, chunk := range []int{1, 2, 7, 26, 27, 100, len(stream)} {
		res := run(t, stream, chunk)
		if len(res) != len(want) {
			t.Fatalf("chunk=%d: пакетов %d, ждали %d", chunk, len(res), len(want))
		}
		for i := range want {
			if !bytes.Equal(res[i].data, want[i]) {
				t.Fatalf("chunk=%d: пакет %d искажён (%d Б, ждали %d Б)", chunk, i, len(res[i].data), len(want[i]))
			}
		}
		// гранула относится к последнему завершённому пакету страницы, EOS — только ко второй странице
		if !res[0].last || res[0].eos || res[1].last || res[2].last || !res[3].last || !res[3].eos || res[3].granule != 4800 {
			t.Fatalf("chunk=%d: флаги LastOnPage/EOS/Granule: %+v", chunk, res)
		}
	}
}

func TestLostStartOfPacketIsDropped(t *testing.T) {
	// Поток начинается со страницы-продолжения: хвост неизвестного пакета выбрасывается, следующий пакет цел.
	tail, next := pattern(20, 5), pattern(30, 6)
	segs := append(lacing(len(tail)), lacing(len(next))...)
	stream := page(flagContinued, 960, segs, append(append([]byte(nil), tail...), next...))
	res := run(t, stream, 5)
	if len(res) != 1 || !bytes.Equal(res[0].data, next) {
		t.Fatalf("получили %d пакетов, ждали один (next)", len(res))
	}
}

func TestTruncatedPacketIsDroppedWhenNextPageIsNotContinuation(t *testing.T) {
	head := pattern(255, 9)
	p1 := page(0, -1, []byte{255}, head) // пакет не закончен...
	next := pattern(40, 4)
	p2 := page(0, 960, lacing(len(next)), next) // ...а следующая страница не продолжение
	res := run(t, append(p1, p2...), 3)
	if len(res) != 1 || !bytes.Equal(res[0].data, next) {
		t.Fatalf("оборванный пакет должен быть выброшен: получили %d пакетов", len(res))
	}
}

func TestSyncError(t *testing.T) {
	var d Demuxer
	err := d.Write(bytes.Repeat([]byte{0x42}, 40), func(Packet) error { return nil })
	if !errors.Is(err, ErrSync) {
		t.Fatalf("ждали ErrSync, получили %v", err)
	}
	if err := d.Write([]byte{1}, func(Packet) error { return nil }); !errors.Is(err, ErrSync) {
		t.Fatal("ошибка должна сохраняться до Reset")
	}
	d.Reset()
	n := 0
	if err := d.Write(page(0, 0, lacing(3), pattern(3, 1)), func(Packet) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("после Reset поток должен читаться: %v, пакетов %d", err, n)
	}
}

func TestEmitErrorStops(t *testing.T) {
	boom := errors.New("boom")
	pk := pattern(3, 1)
	stream := page(0, 0, append(lacing(3), lacing(3)...), append(append([]byte(nil), pk...), pk...))
	var d Demuxer
	calls := 0
	err := d.Write(stream, func(Packet) error { calls++; return boom })
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestBigBufferIsReleased(t *testing.T) {
	// Самый большой пакет, помещающийся на странице: 254 сегмента по 255 + один 254 = 65024 байта.
	// Две такие страницы одним вызовом раздувают буфер выше порога: после разбора он должен отпуститься.
	big := pattern(65024, 1)
	stream := append(page(0, 0, lacing(len(big)), big), page(0, 0, lacing(len(big)), big)...)
	var d Demuxer
	n := 0
	if err := d.Write(stream, func(p Packet) error {
		n++
		if !bytes.Equal(p.Data, big) {
			t.Error("пакет искажён")
		}
		return nil
	}); err != nil || n != 2 {
		t.Fatalf("err=%v пакетов %d", err, n)
	}
	if d.buf != nil {
		t.Errorf("крупный буфер должен отпускаться, cap=%d", cap(d.buf))
	}
}
