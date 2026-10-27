package opusstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"

	opus "github.com/tphakala/go-opus/opus"
)

// Тесты самодостаточны: тон кодируется кодировщиком go-opus и заворачивается в Ogg нашим писателем.
// Реальный голос Яндекса в репозиторий не кладётся; точность на нём проверяет tools/celtcheck.

const frame = 960 // 20 мс

func encodeTone(t *testing.T, freq float64, frames, bitrate int) (pkts [][]byte, preSkip int) {
	t.Helper()
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: SampleRate, Channels: 1, Bitrate: bitrate})
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, frame)
	buf := make([]byte, 1500)
	for f := 0; f < frames; f++ {
		for i := range pcm {
			pcm[i] = int16(8000 * math.Sin(2*math.Pi*freq*float64(f*frame+i)/SampleRate))
		}
		n, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, append([]byte(nil), buf[:n]...))
	}
	return pkts, enc.PreSkip()
}

func opusHead(channels byte, preSkip int, gainQ8 int16, mapping byte) []byte {
	b := []byte("OpusHead")
	b = append(b, 1, channels)
	b = binary.LittleEndian.AppendUint16(b, uint16(preSkip))
	b = binary.LittleEndian.AppendUint32(b, SampleRate)
	b = binary.LittleEndian.AppendUint16(b, uint16(gainQ8))
	return append(b, mapping)
}

func opusTags() []byte {
	b := []byte("OpusTags")
	b = binary.LittleEndian.AppendUint32(b, 4)
	b = append(b, "test"...)
	return binary.LittleEndian.AppendUint32(b, 0)
}

type pkt struct {
	data []byte
	end  int64 // гранула после этого пакета (-1 — не задавать)
}

// oggStream собирает поток из пакетов так же, как настоящий писатель Ogg: сегменты по 255 байт, пакет может
// перетекать между страницами (флаг continued), гранула ставится по последнему завершённому пакету страницы.
func oggStream(pkts []pkt, maxSegs int) []byte {
	type seg struct {
		b       []byte
		endsPkt int // индекс пакета, который завершает этот сегмент, иначе -1
	}
	var segs []seg
	for i, p := range pkts {
		d := p.data
		for len(d) >= 255 {
			segs = append(segs, seg{d[:255], -1})
			d = d[255:]
		}
		segs = append(segs, seg{d, i})
	}
	var out []byte
	cont := false
	for off := 0; off < len(segs); off += maxSegs {
		end := min(off+maxSegs, len(segs))
		part := segs[off:end]
		var flags byte
		if cont {
			flags |= 0x01
		}
		if off == 0 {
			flags |= 0x02
		}
		if end == len(segs) {
			flags |= 0x04
		}
		granule := int64(-1)
		var table, body []byte
		for _, s := range part {
			table = append(table, byte(len(s.b)))
			body = append(body, s.b...)
			if s.endsPkt >= 0 {
				granule = pkts[s.endsPkt].end
			}
		}
		h := make([]byte, 27)
		copy(h, "OggS")
		h[5] = flags
		binary.LittleEndian.PutUint64(h[6:], uint64(granule))
		h[26] = byte(len(table))
		out = append(out, h...)
		out = append(out, table...)
		out = append(out, body...)
		cont = len(part[len(part)-1].b) == 255
	}
	return out
}

func buildTone(t *testing.T, freq float64, frames, bitrate, maxSegs int) (stream []byte, preSkip int) {
	t.Helper()
	pkts, preSkip := encodeTone(t, freq, frames, bitrate)
	list := []pkt{{opusHead(1, preSkip, 0, 0), 0}, {opusTags(), 0}}
	for i, p := range pkts {
		list = append(list, pkt{p, int64((i + 1) * frame)})
	}
	return oggStream(list, maxSegs), preSkip
}

func decodeAll(t *testing.T, s *Stream, stream []byte, chunk int) []byte {
	t.Helper()
	var out []byte
	sink := func(p []byte) error { out = append(out, p...); return nil }
	for i := 0; i < len(stream); i += chunk {
		if err := s.Write(stream[i:min(i+chunk, len(stream))], sink); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func samples(b []byte) []int16 {
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

// power — мощность на частоте f (алгоритм Гёрцеля).
func power(x []int16, f float64) float64 {
	coeff := 2 * math.Cos(2*math.Pi*f/SampleRate)
	var s1, s2 float64
	for _, v := range x {
		s1, s2 = float64(v)+coeff*s1-s2, s1
	}
	return s1*s1 + s2*s2 - coeff*s1*s2
}

func rms(x []int16) float64 {
	var sum float64
	for _, v := range x {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(x)))
}

func TestDecodeToneEveryChunkingAndPaging(t *testing.T) {
	const frames = 50
	for _, bitrate := range []int{48000, 160000} { // 160 кбит/с даёт пакеты >255 Б: сегменты и перетекание
		for _, maxSegs := range []int{255, 3, 1} {
			stream, preSkip := buildTone(t, 440, frames, bitrate, maxSegs)
			s, err := New()
			if err != nil {
				t.Fatal(err)
			}
			var ref []byte
			for _, chunk := range []int{len(stream), 1, 5, 64, 1000} {
				s.Reset()
				out := decodeAll(t, s, stream, chunk)
				if ref == nil {
					ref = out
				} else if !bytes.Equal(out, ref) {
					t.Fatalf("bitrate=%d maxSegs=%d chunk=%d: вывод зависит от нарезки потока", bitrate, maxSegs, chunk)
				}
			}
			if want := (frames*frame - preSkip) * 2; len(ref) != want {
				t.Fatalf("bitrate=%d maxSegs=%d: %d байт, ждали %d (кадры минус pre-skip)", bitrate, maxSegs, len(ref), want)
			}
			x := samples(ref)
			if ratio := power(x, 440) / power(x, 1000); ratio < 1000 {
				t.Fatalf("bitrate=%d maxSegs=%d: на 440 Гц мощности лишь в %.0f раз больше, чем на 1000 Гц", bitrate, maxSegs, ratio)
			}
		}
	}
}

func TestResetReusesDecoderAndIsDeterministic(t *testing.T) {
	stream, _ := buildTone(t, 440, 30, 64000, 255)
	s, _ := New()
	a := decodeAll(t, s, stream, 100)
	s.Reset()
	b := decodeAll(t, s, stream, 100)
	if !bytes.Equal(a, b) || len(a) == 0 {
		t.Fatal("после Reset тот же поток должен давать тот же вывод")
	}
	if s.Samples() != int64(len(b)/2) {
		t.Fatalf("Samples()=%d, ждали %d", s.Samples(), len(b)/2)
	}
}

func TestEndTrimByGranule(t *testing.T) {
	const frames, cut = 20, 500
	pkts, preSkip := encodeTone(t, 440, frames, 64000)
	list := []pkt{{opusHead(1, preSkip, 0, 0), 0}, {opusTags(), 0}}
	for i, p := range pkts {
		end := int64((i + 1) * frame)
		if i == len(pkts)-1 {
			end -= cut // хвост последнего кадра: добивка
		}
		list = append(list, pkt{p, end})
	}
	s, _ := New()
	out := decodeAll(t, s, oggStream(list, 255), 77)
	if want := (frames*frame - cut - preSkip) * 2; len(out) != want {
		t.Fatalf("%d байт, ждали %d", len(out), want)
	}
}

func TestZeroLengthPacketIsSkipped(t *testing.T) {
	pkts, preSkip := encodeTone(t, 440, 10, 64000)
	plain := []pkt{{opusHead(1, preSkip, 0, 0), 0}, {opusTags(), 0}}
	withEmpty := append([]pkt(nil), plain...)
	for i, p := range pkts {
		plain = append(plain, pkt{p, int64((i + 1) * frame)})
		withEmpty = append(withEmpty, pkt{p, -1})
		if i == 4 {
			withEmpty = append(withEmpty, pkt{nil, -1})
		}
	}
	withEmpty[len(withEmpty)-1].end = int64(len(pkts) * frame)
	s, _ := New()
	a := decodeAll(t, s, oggStream(plain, 255), 50)
	s.Reset()
	b := decodeAll(t, s, oggStream(withEmpty, 255), 50)
	if !bytes.Equal(a, b) || len(a) == 0 {
		t.Fatal("пустой пакет не должен влиять на вывод")
	}
}

func TestOutputGain(t *testing.T) {
	pkts, preSkip := encodeTone(t, 440, 20, 64000)
	build := func(gain int16) []byte {
		list := []pkt{{opusHead(1, preSkip, gain, 0), 0}, {opusTags(), 0}}
		for i, p := range pkts {
			list = append(list, pkt{p, int64((i + 1) * frame)})
		}
		return oggStream(list, 255)
	}
	s, _ := New()
	flat := samples(decodeAll(t, s, build(0), 200))
	s.Reset()
	loud := samples(decodeAll(t, s, build(6*256), 200)) // +6 дБ в формате Q7.8
	if r := rms(loud) / rms(flat); math.Abs(r-1.9953) > 0.04 {
		t.Fatalf("+6 дБ должно давать ×1.995 по RMS, получили ×%.3f", r)
	}
}

func TestRejectsUnsupportedAndBrokenHeaders(t *testing.T) {
	cases := []struct {
		name string
		list []pkt
		want error
	}{
		{"стерео", []pkt{{opusHead(2, 312, 0, 0), 0}, {opusTags(), 0}}, ErrUnsupported},
		{"семейство каналов", []pkt{{opusHead(1, 312, 0, 1), 0}, {opusTags(), 0}}, ErrUnsupported},
		{"не Opus", []pkt{{[]byte("NotOpusAtAll......."), 0}}, ErrHead},
		{"короткий заголовок", []pkt{{[]byte("OpusHead"), 0}}, ErrHead},
		{"нет OpusTags", []pkt{{opusHead(1, 312, 0, 0), 0}, {bytes.Repeat([]byte{0xfc}, 40), 0}}, ErrHead},
	}
	for _, c := range cases {
		s, _ := New()
		err := s.Write(oggStream(c.list, 255), func([]byte) error { return nil })
		if !errors.Is(err, c.want) {
			t.Errorf("%s: получили %v, ждали %v", c.name, err, c.want)
		}
	}
	s, _ := New()
	if err := s.Write(bytes.Repeat([]byte{7}, 64), func([]byte) error { return nil }); err == nil {
		t.Error("мусор вместо Ogg должен давать ошибку")
	}
}

func TestSinkErrorStops(t *testing.T) {
	stream, _ := buildTone(t, 440, 10, 64000, 255)
	s, _ := New()
	boom := errors.New("boom")
	calls := 0
	err := s.Write(stream, func([]byte) error { calls++; return boom })
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

// Реальный чанк голоса: OMD_OGG=путь/к/файлу.ogg go test ./internal/opusstream -run Real -v
func TestRealChunk(t *testing.T) {
	path := os.Getenv("OMD_OGG")
	if path == "" {
		t.Skip("OMD_OGG не задан")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New()
	out := decodeAll(t, s, data, 4096)
	if len(out) == 0 {
		t.Fatal("пустой вывод")
	}
	t.Logf("%d отсчётов = %.3f с, RMS %.0f", s.Samples(), float64(s.Samples())/SampleRate, rms(samples(out)))
}

// Один ответ Алисы приходит цепочкой самостоятельных Ogg-потоков (по одному на voice-chunk).
func TestChainedStreams(t *testing.T) {
	a, preA := buildTone(t, 440, 20, 64000, 255)
	b, preB := buildTone(t, 880, 25, 64000, 255)
	nA, nB := 20*frame-preA, 25*frame-preB
	whole := append(append([]byte(nil), a...), b...)

	s, _ := New()
	ref := decodeAll(t, s, whole, len(whole)) // обе цепочки одним куском
	if want := (nA + nB) * 2; len(ref) != want {
		t.Fatalf("%d байт, ждали %d (оба потока без pre-skip)", len(ref), want)
	}
	if s.Samples() != int64(nA+nB) {
		t.Fatalf("Samples()=%d, ждали %d", s.Samples(), nA+nB)
	}
	x := samples(ref)
	if r := power(x[:nA], 440) / power(x[:nA], 880); r < 100 {
		t.Errorf("первая часть должна быть 440 Гц, отношение мощностей %.1f", r)
	}
	if r := power(x[nA:], 880) / power(x[nA:], 440); r < 100 {
		t.Errorf("вторая часть должна быть 880 Гц, отношение мощностей %.1f", r)
	}

	// те же байты по одному
	s.Reset()
	if got := decodeAll(t, s, whole, 1); !bytes.Equal(got, ref) {
		t.Error("побайтовая подача даёт другой вывод")
	}
	// как приходит по шине: каждый поток отдельной командой
	s.Reset()
	var two []byte
	sink := func(p []byte) error { two = append(two, p...); return nil }
	for _, chunk := range [][]byte{a, b} {
		if err := s.Write(chunk, sink); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(two, ref) {
		t.Error("подача двумя командами даёт другой вывод")
	}
	// поток после цепочки: Reset не нужен, новый ответ — это ещё один BOS
	if err := s.Write(a, sink); err != nil {
		t.Fatalf("третий поток подряд: %v", err)
	}
	if s.Samples() != int64(nA+nB+nA) {
		t.Errorf("после третьего потока Samples()=%d, ждали %d", s.Samples(), nA+nB+nA)
	}
}

// PreSkipTap отдаёт выброшенные pre-skip отсчёты, а вместе с основным выводом они дают всё, что декодер выдал бы без подрезки.
func TestPreSkipTapReceivesTrimmedSamples(t *testing.T) {
	stream, preSkip := buildTone(t, 440, 20, 64000, 255)
	s, _ := New()
	var tapped []byte
	s.PreSkipTap = func(pcm []byte) { tapped = append(tapped, pcm...) }
	out := decodeAll(t, s, stream, len(stream))
	if len(tapped) != preSkip*2 {
		t.Fatalf("в PreSkipTap пришло %d байт, ждали pre-skip %d отсчётов = %d байт", len(tapped), preSkip, preSkip*2)
	}
	// без Tap вывод тот же: Tap только подсматривает
	s2, _ := New()
	if got := decodeAll(t, s2, stream, len(stream)); !bytes.Equal(got, out) {
		t.Fatal("PreSkipTap не должен менять основной вывод")
	}
	s.Reset()
	tapped = tapped[:0]
	decodeAll(t, s, stream, 7)
	if len(tapped) != preSkip*2 {
		t.Fatalf("после Reset и другой нарезки в Tap %d байт, ждали %d", len(tapped), preSkip*2)
	}
}
