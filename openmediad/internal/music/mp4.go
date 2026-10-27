// Package music — воспроизведение потоков Яндекс.Музыки своими силами: скачивание, расшифровка AES-CTR, разбор MP4,
// декодирование AAC (GStreamer колонки), вывод в ALSA. Нужен фазе 3, когда шим сам играет музыку вместо mediad.
package music

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Index — таблица сэмплов AAC из moov обычного (не фрагментированного) MP4: по ней можно прыгнуть в любое место
// потока HTTP-запросом Range и отдать декодеру голые кадры AAC, обернув их в ADTS.
type Index struct {
	DurationMs uint64 // mvhd
	Timescale  uint32 // mdhd: частота дискретизации
	ASC        []byte // AudioSpecificConfig из esds
	Off        []uint64
	Size       []uint32
	cum        []uint64 // накопленная длительность до сэмпла i в единицах Timescale (len = n+1)
	MoovEnd    uint64   // смещение первого байта после moov
}

// N — число кадров AAC в треке.
func (x *Index) N() int { return len(x.Size) }

// SampleAtMs возвращает номер кадра, который звучит в момент ms.
func (x *Index) SampleAtMs(ms uint64) int {
	t := ms * uint64(x.Timescale) / 1000
	lo, hi := 0, x.N()
	for lo < hi {
		mid := (lo + hi) / 2
		if x.cum[mid+1] <= t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= x.N() {
		lo = x.N() - 1
	}
	return lo
}

// StartMs — время начала кадра i, мс.
func (x *Index) StartMs(i int) uint64 { return x.cum[i] * 1000 / uint64(x.Timescale) }

// ReadMoov читает из начала потока боксы до конца moov (ftyp, uuid и т. п. пропускаются) и разбирает moov.
// Возвращает таблицу и число прочитанных байт; дальше в потоке идёт mdat.
func ReadMoov(r io.Reader) (*Index, error) {
	var off uint64
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("mp4: заголовок бокса: %w", err)
		}
		size := uint64(binary.BigEndian.Uint32(h[:4]))
		typ := string(h[4:])
		hdr := uint64(8)
		if size == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return nil, err
			}
			size, hdr = binary.BigEndian.Uint64(ext[:]), 16
		}
		if size < hdr || size > 64<<20 {
			return nil, fmt.Errorf("mp4: бокс %q размера %d", typ, size)
		}
		if typ == "mdat" {
			return nil, errors.New("mp4: mdat раньше moov — перемотка недоступна")
		}
		body := size - hdr
		if typ != "moov" {
			if _, err := io.CopyN(io.Discard, r, int64(body)); err != nil {
				return nil, fmt.Errorf("mp4: пропуск бокса %q: %w", typ, err)
			}
			off += size
			continue
		}
		buf := make([]byte, body)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("mp4: moov: %w", err)
		}
		x, err := parseMoov(buf)
		if err != nil {
			return nil, err
		}
		x.MoovEnd = off + size
		return x, nil
	}
}

type box struct {
	typ  string
	body []byte
}

func children(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b[:4]))
		typ := string(b[4:8])
		hdr := uint64(8)
		if size == 1 && len(b) >= 16 {
			size, hdr = binary.BigEndian.Uint64(b[8:16]), 16
		}
		if size == 0 {
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			break
		}
		out = append(out, box{typ, b[hdr:size]})
		b = b[size:]
	}
	return out
}

func find(bs []box, typ string) []byte {
	for _, b := range bs {
		if b.typ == typ {
			return b.body
		}
	}
	return nil
}

func path(b []byte, typs ...string) []byte {
	for _, t := range typs {
		b = find(children(b), t)
		if b == nil {
			return nil
		}
	}
	return b
}

func parseMoov(moov []byte) (*Index, error) {
	x := &Index{}
	mvhd := path(moov, "mvhd")
	if len(mvhd) < 20 {
		return nil, errors.New("mp4: нет mvhd")
	}
	if mvhd[0] == 0 {
		ts, dur := binary.BigEndian.Uint32(mvhd[12:]), binary.BigEndian.Uint32(mvhd[16:])
		x.DurationMs = uint64(dur) * 1000 / uint64(ts)
	} else {
		ts, dur := binary.BigEndian.Uint32(mvhd[20:]), binary.BigEndian.Uint64(mvhd[24:])
		x.DurationMs = dur * 1000 / uint64(ts)
	}
	var stbl, mdhd []byte
	for _, tr := range children(moov) {
		if tr.typ != "trak" {
			continue
		}
		mdia := path(tr.body, "mdia")
		if hd := path(mdia, "hdlr"); len(hd) >= 12 && string(hd[8:12]) == "soun" {
			stbl = path(mdia, "minf", "stbl")
			mdhd = path(mdia, "mdhd")
			break
		}
	}
	if stbl == nil || len(mdhd) < 20 {
		return nil, errors.New("mp4: нет звуковой дорожки")
	}
	if mdhd[0] == 0 {
		x.Timescale = binary.BigEndian.Uint32(mdhd[12:])
	} else {
		x.Timescale = binary.BigEndian.Uint32(mdhd[20:])
	}
	if x.Timescale == 0 {
		return nil, errors.New("mp4: нулевой timescale")
	}
	st := children(stbl)
	var err error
	if x.ASC, err = parseESDS(find(st, "stsd")); err != nil {
		return nil, err
	}
	sizes, err := parseSTSZ(find(st, "stsz"))
	if err != nil {
		return nil, err
	}
	x.Size = sizes
	x.Off, err = parseOffsets(find(st, "stco"), find(st, "co64"), find(st, "stsc"), sizes)
	if err != nil {
		return nil, err
	}
	x.cum, err = parseSTTS(find(st, "stts"), len(sizes))
	return x, err
}
