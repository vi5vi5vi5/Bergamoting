package music

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func mk(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func u32(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

// synthMP4 собирает небольшой MP4 с AAC-LC 48 кГц стерео: n кадров по 1024, размеры size(i), два кадра в чанке.
func synthMP4(n int, size func(i int) uint32) (file []byte, payload [][]byte) {
	asc := []byte{0x11, 0x90} // AAC-LC, 48 кГц, стерео
	dsi := append([]byte{0x05, byte(len(asc))}, asc...)
	dcd := append([]byte{0x04, byte(13 + len(dsi)), 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, dsi...)
	sl := []byte{0x06, 1, 2}
	esd := append(append([]byte{0x03, byte(3 + len(dcd) + len(sl)), 0, 1, 0}, dcd...), sl...)
	esds := mk("esds", u32(0), esd)
	mp4a := mk("mp4a", make([]byte, 28), esds)
	stsd := mk("stsd", u32(0, 1), mp4a)
	stts := mk("stts", u32(0, 1, uint32(n), 1024))
	stsc := mk("stsc", u32(0, 1, 1, 2, 1))
	sizes := make([]uint32, n)
	for i := range sizes {
		sizes[i] = size(i)
	}
	stsz := mk("stsz", u32(0, 0, uint32(n)), u32(sizes...))
	chunks := (n + 1) / 2
	mdatStart := uint32(0)
	build := func(start uint32) []byte {
		offs := make([]uint32, 0, chunks)
		pos := start
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				offs = append(offs, pos)
			}
			pos += sizes[i]
		}
		stco := mk("stco", u32(0, uint32(len(offs))), u32(offs...))
		stbl := mk("stbl", stsd, stts, stsc, stsz, stco)
		mdhd := mk("mdhd", u32(0, 0, 0, 48000, uint32(n*1024)), []byte{0, 0, 0, 0})
		hdlr := mk("hdlr", u32(0, 0), []byte("soun"), make([]byte, 12))
		mdia := mk("mdia", mdhd, hdlr, mk("minf", stbl))
		mvhd := mk("mvhd", u32(0, 0, 0, 1000, uint32(uint64(n)*1024*1000/48000)), make([]byte, 80))
		moov := mk("moov", mvhd, mk("trak", mdia))
		return append(mk("ftyp", []byte("isom"), u32(0)), moov...)
	}
	head := build(0)
	mdatStart = uint32(len(head)) + 8
	head = build(mdatStart)
	var data []byte
	for i := 0; i < n; i++ {
		p := bytes.Repeat([]byte{byte(i)}, int(sizes[i]))
		payload = append(payload, p)
		data = append(data, p...)
	}
	return append(head, mk("mdat", data)...), payload
}

func TestReadMoovSynthetic(t *testing.T) {
	file, payload := synthMP4(7, func(i int) uint32 { return uint32(100 + 10*i) })
	x, err := ReadMoov(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if x.N() != 7 || x.Timescale != 48000 {
		t.Fatalf("N=%d timescale=%d", x.N(), x.Timescale)
	}
	if want := uint64(7 * 1024 * 1000 / 48000); x.DurationMs != want {
		t.Fatalf("длительность %d мс, ждали %d", x.DurationMs, want)
	}
	if !bytes.Equal(x.ASC, []byte{0x11, 0x90}) {
		t.Fatalf("ASC = %x", x.ASC)
	}
	for i := 0; i < 7; i++ {
		got := file[x.Off[i] : x.Off[i]+uint64(x.Size[i])]
		if !bytes.Equal(got, payload[i]) {
			t.Fatalf("кадр %d: смещение %d указывает не туда", i, x.Off[i])
		}
	}
	if x.MoovEnd >= x.Off[0] {
		t.Fatalf("moov заканчивается на %d, первый кадр на %d", x.MoovEnd, x.Off[0])
	}
	// 1024 кадра/48000 = 21,333 мс: кадр 3 начинается на 64 мс, 4-й на 85,3
	if i := x.SampleAtMs(70); i != 3 {
		t.Fatalf("на 70 мс звучит кадр 3, а не %d", i)
	}
	if i := x.SampleAtMs(10_000_000); i != 6 {
		t.Fatalf("за концом — последний кадр, а не %d", i)
	}
	if x.StartMs(3) != 64 {
		t.Fatalf("кадр 3 начинается на %d мс, ждали 64", x.StartMs(3))
	}
}

func TestADTSHeader(t *testing.T) {
	h, err := ADTS([]byte{0x11, 0x90}, 371)
	if err != nil {
		t.Fatal(err)
	}
	// 48 кГц (индекс 3), 2 канала, LC: ff f1 4c 80 | длина 378 → 0x2f 0x5f 0xfc
	want := [7]byte{0xff, 0xf1, 0x4c, 0x80, 0x2f, 0x5f, 0xfc}
	if h != want {
		t.Fatalf("ADTS %x, ждали %x", h, want)
	}
	if _, err := ADTS([]byte{0x11}, 1); err == nil {
		t.Fatal("короткий ASC должен давать ошибку")
	}
	if _, err := ADTS([]byte{0x29, 0x90}, 1); err == nil { // объект 5 (HE-AAC) не поддерживается
		t.Fatal("HE-AAC без SBR-разбора должен отвергаться")
	}
}

// TestRealTrack — на настоящем потоке Яндекса (расшифрованный MP4), если путь задан: OMD_TRACK_MP4.
func TestRealTrack(t *testing.T) {
	path := os.Getenv("OMD_TRACK_MP4")
	if path == "" {
		t.Skip("OMD_TRACK_MP4 не задан")
	}
	f, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	x, err := ReadMoov(bytes.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("кадров %d, длительность %d мс, timescale %d, ASC %x, moov до %d, первый кадр на %d", x.N(), x.DurationMs, x.Timescale, x.ASC, x.MoovEnd, x.Off[0])
	var end uint64
	for i := 0; i < x.N(); i++ {
		if i > 0 && x.Off[i] < x.Off[i-1] {
			t.Fatalf("смещения не растут на кадре %d", i)
		}
		if e := x.Off[i] + uint64(x.Size[i]); e > end {
			end = e
		}
		if _, err := ADTS(x.ASC, int(x.Size[i])); err != nil {
			t.Fatal(err)
		}
	}
	if end > uint64(len(f)) {
		t.Fatalf("последний кадр заканчивается на %d за концом файла %d", end, len(f))
	}
	if got := x.StartMs(x.N()); got < x.DurationMs-100 || got > x.DurationMs+100 {
		t.Fatalf("сумма длительностей кадров %d мс не сходится с mvhd %d мс", got, x.DurationMs)
	}
}
