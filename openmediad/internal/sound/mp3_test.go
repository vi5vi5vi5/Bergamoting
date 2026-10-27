package sound

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMp3Bitrate(t *testing.T) {
	// Начало настоящего timer.wav с колонки: MPEG1 layer III, 64 кбит/с, затем тег Xing.
	real := []byte{0xff, 0xfb, 0x50, 0xc4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x49, 0x6e, 0x66, 0x6f}
	tag := func(size int) []byte { // ID3v2 с телом заданного размера
		b := []byte{'I', 'D', '3', 3, 0, 0, byte(size >> 21 & 0x7f), byte(size >> 14 & 0x7f), byte(size >> 7 & 0x7f), byte(size & 0x7f)}
		return append(b, make([]byte, size)...)
	}
	cases := []struct {
		name string
		data []byte
		want uint64
	}{
		{"настоящий timer.wav", real, 64},
		{"MPEG1 128", []byte{0xff, 0xfb, 0x90, 0x00}, 128},
		{"MPEG2 64 (индекс 8)", []byte{0xff, 0xf3, 0x80, 0x00}, 64},
		{"после тега ID3", append(tag(300), real...), 64},
		{"мусор перед кадром", append([]byte{1, 2, 0xff, 0x00, 3}, real...), 64},
		{"layer II не берём", []byte{0xff, 0xfd, 0x90, 0x00}, 0},
		{"индекс 15 запрещён", []byte{0xff, 0xfb, 0xf0, 0x00}, 0},
		{"индекс 0 (free) не берём", []byte{0xff, 0xfb, 0x00, 0x00}, 0},
		{"не MP3", []byte("OggS\x00\x02" + string(make([]byte, 40))), 0},
		{"пусто", nil, 0},
		{"обрыв на заголовке", []byte{0xff, 0xfb, 0x50}, 0},
	}
	for _, c := range cases {
		if got := mp3Bitrate(c.data); got != c.want {
			t.Errorf("%s: %d, ждали %d", c.name, got, c.want)
		}
	}
}

func TestFileBitrateSkipsHugeID3Tag(t *testing.T) {
	dir := t.TempDir()
	// Тег больше окна чтения (4096): кадр лежит дальше, и его надо найти переходом по размеру тега.
	size := 20000
	b := []byte{'I', 'D', '3', 3, 0, 0, byte(size >> 21 & 0x7f), byte(size >> 14 & 0x7f), byte(size >> 7 & 0x7f), byte(size & 0x7f)}
	b = append(b, bytes.Repeat([]byte{0xff, 0x00}, size/2)...) // в теге попадаются «ложные синхрослова»
	b = append(b, 0xff, 0xfb, 0x50, 0xc4, 0, 0, 0, 0)
	path := filepath.Join(dir, "x.wav")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FileBitrate(path); got != 64 {
		t.Fatalf("FileBitrate = %d, ждали 64", got)
	}
	if FileBitrate(filepath.Join(dir, "нет.wav")) != 0 {
		t.Fatal("несуществующий файл должен дать 0")
	}
}
