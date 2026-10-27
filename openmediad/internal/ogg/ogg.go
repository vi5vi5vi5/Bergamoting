// Package ogg — потоковый разборщик контейнера Ogg (RFC 3533): принимает байты кусками любого размера
// и выдаёт завершённые пакеты. Контрольная сумма страниц не проверяется: поток идёт по loopback внутри
// колонки, и лишние циклы на нём не нужны.
package ogg

import (
	"encoding/binary"
	"errors"
)

const (
	flagContinued = 0x01
	flagBOS       = 0x02
	flagEOS       = 0x04
	headerLen     = 27
	keepCap       = 64 << 10
)

var ErrSync = errors.New("ogg: нет сигнатуры OggS")

// Packet — завершённый пакет.
type Packet struct {
	Data       []byte // действителен только внутри вызова emit
	Granule    int64  // позиция страницы; относится к последнему завершённому пакету страницы (см. LastOnPage)
	LastOnPage bool   // пакет — последний завершённый на своей странице, значит Granule к нему применим
	BOS, EOS   bool   // флаги страницы
}

// Demuxer хранит незавершённый хвост между вызовами Write. Нулевое значение готово к работе.
type Demuxer struct {
	buf     []byte // необработанный хвост (неполная страница)
	pkt     []byte // пакет, переходящий через сегменты и страницы
	open    bool   // pkt содержит начало пакета, который ещё не завершён
	discard bool   // идёт хвост пакета, начала которого мы не видели: его нужно выбросить
	err     error
}

// Reset готовит демуксер к новому потоку.
func (d *Demuxer) Reset() {
	d.buf, d.pkt = d.buf[:0], d.pkt[:0]
	d.open, d.discard, d.err = false, false, nil
}

// Write добавляет байты потока и для каждого завершённого пакета вызывает emit.
// Ошибка emit прерывает разбор и возвращается; после ошибки потока Write возвращает её же, пока не вызван Reset.
func (d *Demuxer) Write(p []byte, emit func(Packet) error) error {
	if d.err != nil {
		return d.err
	}
	d.buf = append(d.buf, p...)
	for len(d.buf) >= headerLen {
		if string(d.buf[:4]) != "OggS" {
			d.err = ErrSync
			return d.err
		}
		hl := headerLen + int(d.buf[26])
		if len(d.buf) < hl {
			break
		}
		body := 0
		for _, s := range d.buf[headerLen:hl] {
			body += int(s)
		}
		if len(d.buf) < hl+body {
			break
		}
		if err := d.page(d.buf[:hl], d.buf[hl:hl+body], emit); err != nil {
			d.err = err
			return err
		}
		n := copy(d.buf, d.buf[hl+body:])
		d.buf = d.buf[:n]
	}
	if len(d.buf) == 0 && cap(d.buf) > keepCap {
		d.buf = nil
	}
	return nil
}

func (d *Demuxer) page(hdr, body []byte, emit func(Packet) error) error {
	flags := hdr[5]
	granule := int64(binary.LittleEndian.Uint64(hdr[6:14]))
	segs := hdr[headerLen:]

	cont := flags&flagContinued != 0
	switch {
	case !cont && d.open: // предыдущий пакет оборвался: его конец потерян
		d.pkt, d.open, d.discard = d.pkt[:0], false, false
	case cont && !d.open: // страница продолжает пакет, начала которого мы не видели
		d.discard = true
	}

	last := -1
	for i, s := range segs {
		if s < 255 {
			last = i
		}
	}
	off := 0
	for i, s := range segs {
		if !d.discard {
			d.pkt = append(d.pkt, body[off:off+int(s)]...)
		}
		off += int(s)
		if s == 255 {
			d.open = true
			continue
		}
		d.open = false
		if d.discard {
			d.discard = false
			continue
		}
		pk := Packet{Data: d.pkt, Granule: granule, LastOnPage: i == last,
			BOS: flags&flagBOS != 0, EOS: flags&flagEOS != 0}
		err := emit(pk)
		d.pkt = d.pkt[:0]
		if err != nil {
			return err
		}
	}
	return nil
}
