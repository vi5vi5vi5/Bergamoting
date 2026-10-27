// Package opusstream — декодер потока Ogg/Opus (RFC 7845) в сырой PCM S16LE для вывода на колонке.
//
// Голос Алисы приходит готовым Ogg/Opus: моно, 48 кГц, режим CELT. Поток держит ОДИН долгоживущий
// декодер: между ответами достаточно Reset(). Новый декодер на каждый ответ раздувает кучу (замер на
// колонке: ≈3 МБ против ≈5 МБ), а в установившемся режиме декодирование почти не выделяет память.
package opusstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	opus "github.com/tphakala/go-opus/opus"

	"openmediad/internal/ogg"
)

const (
	SampleRate = 48000
	maxFrame   = 5760 // 120 мс при 48 кГц — самый длинный кадр Opus
)

var (
	ErrHead        = errors.New("opusstream: некорректный OpusHead/OpusTags")
	ErrUnsupported = errors.New("opusstream: неподдерживаемый поток")
)

// Stream декодирует Ogg/Opus. Не потокобезопасен.
//
// Один ответ Алисы — это ЦЕПОЧКА самостоятельных Ogg-потоков: каждый voice-chunk имеет свои страницы BOS/EOS,
// свой OpusHead/OpusTags, свой серийный номер и свои гранулы (замер: длинный ответ пришёл двумя потоками по ≈2 с).
// Новый поток определяется по флагу BOS у страницы с OpusHead; тогда декодер сбрасывается (RFC 7845) и счётчики
// подрезки по гранулe начинаются заново.
type Stream struct {
	dem     ogg.Demuxer
	dec     *opus.Decoder
	state   int     // 0 — ждём OpusHead, 1 — ждём OpusTags, 2 — аудио
	preSkip int     // из OpusHead: сколько отсчётов в начале выбросить
	skip    int     // сколько ещё осталось выбросить
	gain    float64 // множитель из OutputGain (1 — без изменений)
	emitted int64   // отсчётов выдано в текущем потоке (после pre-skip): от него считается подрезка по гранулe
	total   int64   // отсчётов выдано всего с последнего Reset
	packets int     // аудиопакетов разобрано (для сообщений об ошибках)
	pcm     [maxFrame]int16
	out     []byte
}

// New создаёт декодер (моно, 48 кГц).
func New() (*Stream, error) {
	dec, err := opus.NewDecoder(SampleRate, 1)
	if err != nil {
		return nil, err
	}
	s := &Stream{dec: dec, out: make([]byte, 0, maxFrame*2)}
	s.Reset()
	return s, nil
}

// Reset готовит поток к новому ответу, сохраняя выделенный декодер.
func (s *Stream) Reset() {
	s.dem.Reset()
	s.dec.Reset()
	s.state, s.preSkip, s.skip, s.gain, s.emitted, s.total, s.packets = 0, 0, 0, 1, 0, 0, 0
}

// beginStream начинает новый логический поток цепочки: состояние и декодер как после Reset, кроме общего счёта.
func (s *Stream) beginStream() {
	s.dec.Reset()
	s.state, s.preSkip, s.skip, s.gain, s.emitted = 0, 0, 0, 1, 0
}

// Samples — сколько отсчётов выдано с последнего Reset (по всем потокам цепочки).
func (s *Stream) Samples() int64 { return s.total }

// Write принимает очередной кусок Ogg-потока (границы кусков могут падать куда угодно) и для каждого
// декодированного пакета вызывает sink с PCM S16LE моно. Срез действителен только внутри вызова.
func (s *Stream) Write(chunk []byte, sink func(pcm []byte) error) error {
	return s.dem.Write(chunk, func(p ogg.Packet) error { return s.packet(p, sink) })
}

func (s *Stream) packet(p ogg.Packet, sink func([]byte) error) error {
	if p.BOS && bytes.HasPrefix(p.Data, []byte("OpusHead")) {
		s.beginStream()
	}
	switch s.state {
	case 0:
		if err := s.head(p.Data); err != nil {
			return err
		}
		s.state = 1
		return nil
	case 1:
		if !bytes.HasPrefix(p.Data, []byte("OpusTags")) {
			return fmt.Errorf("%w: вторым пакетом ждали OpusTags", ErrHead)
		}
		s.state = 2
		return nil
	}
	if len(p.Data) == 0 {
		return nil
	}
	s.packets++
	n, err := s.dec.Decode(p.Data, s.pcm[:])
	if err != nil {
		return fmt.Errorf("opusstream: пакет %d: %w", s.packets, err)
	}
	samples := s.pcm[:n]
	if s.skip > 0 {
		k := min(s.skip, len(samples))
		samples, s.skip = samples[k:], s.skip-k
	}
	// Гранула последней страницы потока задаёт точную длину: всё, что за ней, — добивка кадра.
	if p.EOS && p.LastOnPage && p.Granule >= 0 {
		allowed := p.Granule - int64(s.preSkip) - s.emitted
		if allowed < 0 {
			allowed = 0
		}
		if int64(len(samples)) > allowed {
			samples = samples[:allowed]
		}
	}
	if len(samples) == 0 {
		return nil
	}
	s.out = s.out[:0]
	for _, v := range samples {
		if s.gain != 1 {
			v = int16(math.Max(-32768, math.Min(32767, math.Round(float64(v)*s.gain))))
		}
		s.out = binary.LittleEndian.AppendUint16(s.out, uint16(v))
	}
	s.emitted += int64(len(samples))
	s.total += int64(len(samples))
	return sink(s.out)
}

func (s *Stream) head(b []byte) error {
	if len(b) < 19 || !bytes.HasPrefix(b, []byte("OpusHead")) {
		return fmt.Errorf("%w: первым пакетом ждали OpusHead", ErrHead)
	}
	if b[8]>>4 != 0 {
		return fmt.Errorf("%w: версия %d", ErrUnsupported, b[8])
	}
	if b[9] != 1 {
		return fmt.Errorf("%w: каналов %d, поддерживается только моно", ErrUnsupported, b[9])
	}
	if b[18] != 0 {
		return fmt.Errorf("%w: семейство отображения каналов %d", ErrUnsupported, b[18])
	}
	s.preSkip = int(binary.LittleEndian.Uint16(b[10:12]))
	s.skip = s.preSkip
	if q8 := int16(binary.LittleEndian.Uint16(b[16:18])); q8 != 0 {
		s.gain = math.Pow(10, float64(q8)/(20*256))
	}
	return nil
}
