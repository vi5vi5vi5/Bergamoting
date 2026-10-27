package music

import (
	"encoding/binary"
	"errors"
)

func parseSTSZ(b []byte) ([]uint32, error) {
	if len(b) < 12 {
		return nil, errors.New("mp4: нет stsz")
	}
	uniform, n := binary.BigEndian.Uint32(b[4:]), int(binary.BigEndian.Uint32(b[8:]))
	if n < 0 || n > 4<<20 {
		return nil, errors.New("mp4: stsz: странное число сэмплов")
	}
	out := make([]uint32, n)
	if uniform != 0 {
		for i := range out {
			out[i] = uniform
		}
		return out, nil
	}
	if len(b) < 12+4*n {
		return nil, errors.New("mp4: stsz обрезан")
	}
	for i := range out {
		out[i] = binary.BigEndian.Uint32(b[12+4*i:])
	}
	return out, nil
}

func parseOffsets(stco, co64, stsc []byte, sizes []uint32) ([]uint64, error) {
	var chunks []uint64
	switch {
	case len(stco) >= 8:
		n := int(binary.BigEndian.Uint32(stco[4:]))
		if len(stco) < 8+4*n {
			return nil, errors.New("mp4: stco обрезан")
		}
		for i := 0; i < n; i++ {
			chunks = append(chunks, uint64(binary.BigEndian.Uint32(stco[8+4*i:])))
		}
	case len(co64) >= 8:
		n := int(binary.BigEndian.Uint32(co64[4:]))
		if len(co64) < 8+8*n {
			return nil, errors.New("mp4: co64 обрезан")
		}
		for i := 0; i < n; i++ {
			chunks = append(chunks, binary.BigEndian.Uint64(co64[8+8*i:]))
		}
	default:
		return nil, errors.New("mp4: нет stco/co64")
	}
	if len(stsc) < 8 {
		return nil, errors.New("mp4: нет stsc")
	}
	ne := int(binary.BigEndian.Uint32(stsc[4:]))
	if ne == 0 || len(stsc) < 8+12*ne {
		return nil, errors.New("mp4: stsc обрезан")
	}
	type run struct{ first, per uint32 }
	runs := make([]run, ne)
	for i := range runs {
		runs[i] = run{binary.BigEndian.Uint32(stsc[8+12*i:]), binary.BigEndian.Uint32(stsc[12+12*i:])}
	}
	out := make([]uint64, 0, len(sizes))
	s := 0
	for c := range chunks {
		per := runs[0].per
		for _, r := range runs {
			if uint32(c+1) >= r.first {
				per = r.per
			}
		}
		pos := chunks[c]
		for k := uint32(0); k < per && s < len(sizes); k++ {
			out = append(out, pos)
			pos += uint64(sizes[s])
			s++
		}
	}
	if s != len(sizes) {
		return nil, errors.New("mp4: таблицы stsc/stco не покрывают все сэмплы")
	}
	return out, nil
}

// parseSTTS возвращает накопленные времена начала кадров (len = n+1).
func parseSTTS(b []byte, n int) ([]uint64, error) {
	if len(b) < 8 {
		return nil, errors.New("mp4: нет stts")
	}
	ne := int(binary.BigEndian.Uint32(b[4:]))
	if len(b) < 8+8*ne {
		return nil, errors.New("mp4: stts обрезан")
	}
	cum := make([]uint64, 1, n+1)
	for i := 0; i < ne; i++ {
		cnt, delta := binary.BigEndian.Uint32(b[8+8*i:]), binary.BigEndian.Uint32(b[12+8*i:])
		for k := uint32(0); k < cnt && len(cum) <= n; k++ {
			cum = append(cum, cum[len(cum)-1]+uint64(delta))
		}
	}
	if len(cum) != n+1 {
		return nil, errors.New("mp4: stts не покрывает все сэмплы")
	}
	return cum, nil
}

// parseESDS достаёт AudioSpecificConfig из stsd → mp4a → esds.
func parseESDS(stsd []byte) ([]byte, error) {
	if len(stsd) < 8 {
		return nil, errors.New("mp4: нет stsd")
	}
	for _, ent := range children(stsd[8:]) {
		// у mp4a перед вложенными боксами 28 байт AudioSampleEntry
		if ent.typ != "mp4a" || len(ent.body) < 28 {
			continue
		}
		esds := find(children(ent.body[28:]), "esds")
		if len(esds) < 4 {
			continue
		}
		return descConfig(esds[4:])
	}
	return nil, errors.New("mp4: нет esds (не AAC?)")
}

func descLen(b []byte) (n, used int) {
	for used < 4 && used < len(b) {
		c := b[used]
		used++
		n = n<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			return n, used
		}
	}
	return 0, 0
}

// descConfig идёт по дескрипторам ES (0x03) → DecoderConfig (0x04) → DecSpecificInfo (0x05).
func descConfig(b []byte) ([]byte, error) {
	for len(b) > 1 {
		tag := b[0]
		n, u := descLen(b[1:])
		if u == 0 || 1+u+n > len(b) {
			break
		}
		body := b[1+u : 1+u+n]
		switch tag {
		case 0x03:
			if len(body) < 3 {
				return nil, errors.New("mp4: короткий ES_Descriptor")
			}
			skip := 3
			if body[2]&0x80 != 0 {
				skip += 2
			}
			if body[2]&0x40 != 0 && len(body) > skip {
				skip += 1 + int(body[skip])
			}
			if body[2]&0x20 != 0 {
				skip += 2
			}
			if skip > len(body) {
				return nil, errors.New("mp4: ES_Descriptor обрезан")
			}
			return descConfig(body[skip:])
		case 0x04:
			if len(body) < 13 {
				return nil, errors.New("mp4: короткий DecoderConfigDescriptor")
			}
			return descConfig(body[13:])
		case 0x05:
			return body, nil
		}
		b = b[1+u+n:]
	}
	return nil, errors.New("mp4: нет DecoderSpecificInfo")
}

// ADTS строит заголовок ADTS для кадра AAC размером size байт по AudioSpecificConfig.
func ADTS(asc []byte, size int) ([7]byte, error) {
	var h [7]byte
	if len(asc) < 2 {
		return h, errors.New("adts: короткий AudioSpecificConfig")
	}
	obj := int(asc[0] >> 3)
	freq := int(asc[0]&7)<<1 | int(asc[1]>>7)
	ch := int(asc[1]>>3) & 0xf
	if obj == 0 || obj > 4 || freq > 12 || ch == 0 || ch > 7 {
		return h, errors.New("adts: профиль/частота/каналы вне поддержки (нужен AAC-LC)")
	}
	l := size + 7
	h[0] = 0xff
	h[1] = 0xf1 // MPEG-4, layer 0, без CRC
	h[2] = byte(obj-1)<<6 | byte(freq)<<2 | byte(ch>>2)
	h[3] = byte(ch&3)<<6 | byte(l>>11)&3
	h[4] = byte(l >> 3)
	h[5] = byte(l&7)<<5 | 0x1f
	h[6] = 0xfc
	return h, nil
}
