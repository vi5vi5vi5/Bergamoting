package sound

import (
	"io"
	"os"
)

// Таблицы битрейтов MPEG audio layer III (кбит/с) по индексу из заголовка кадра.
var (
	mpeg1L3 = [16]uint64{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mpeg2L3 = [16]uint64{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
)

// mp3Bitrate — битрейт первого кадра MPEG layer III в data, кбит/с; 0, если кадра нет. Тег ID3v2 в начале пропускается
// по его размеру (если он целиком в data). mediad показывает этот битрейт платформе в diag событий (у timer.wav — 64).
func mp3Bitrate(data []byte) uint64 {
	i := 0
	if len(data) >= 10 && string(data[:3]) == "ID3" {
		i = 10 + id3Size(data[6:10])
	}
	for ; i+4 <= len(data); i++ {
		if data[i] != 0xFF || data[i+1]&0xE0 != 0xE0 {
			continue
		}
		ver := (data[i+1] >> 3) & 3   // 3 — MPEG1, 2 — MPEG2, 0 — MPEG2.5, 1 — зарезервировано
		layer := (data[i+1] >> 1) & 3 // 1 — layer III
		idx := data[i+2] >> 4
		if ver == 1 || layer != 1 || idx == 0 || idx == 15 {
			continue
		}
		if ver == 3 {
			return mpeg1L3[idx]
		}
		return mpeg2L3[idx]
	}
	return 0
}

// id3Size — размер тега ID3v2 без заголовка: четыре байта по 7 бит.
func id3Size(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

// FileBitrate читает начало файла и возвращает битрейт MP3 (0 — не MP3 или не удалось прочитать).
func FileBitrate(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	head := make([]byte, 10)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0
	}
	skip := int64(0)
	if string(head[:3]) == "ID3" {
		skip = int64(10 + id3Size(head[6:10]))
	}
	if _, err := f.Seek(skip, io.SeekStart); err != nil {
		return 0
	}
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	return mp3Bitrate(buf[:n])
}
