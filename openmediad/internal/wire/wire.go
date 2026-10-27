// Package wire — транспорт шины Quasar: кадры [u32 LE длина][QuasarMessage] и конверт {252: тело, 253: тип}.
//
// QuasarMessage — «оболочка», где номер поля = тип вложенного сообщения. На провод кладут только два
// поля: 252 (сериализованный payload) и 253 (номер типа). Подтверждено на всех сервисах шины.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"openmediad/internal/pb"
)

// MaxFrame — лимит прошивки на размер кадра (IPC_MAX_FRAME_SIZE, 10 МиБ).
const MaxFrame = 10 << 20

// keepBuf — буфер крупнее этого отпускается после чтения большого кадра, чтобы не держать RSS.
const keepBuf = 64 << 10

// Type — номер типа payload в конверте (поле 253).
type Type uint64

// Типы, подтверждённые трафиком (см. quasar messaging/PROTOCOL.md, §2.2).
const (
	TypeAliceState   Type = 3   // aliced: состояние Алисы и распознанный текст
	TypeAudioCommand Type = 8   // audioclient: команда плееру (platformd → mediad)
	TypeIOHub        Type = 85  // iohub: SDKState и Spectrum
	TypeIOHubReply   Type = 88  // iohub: ответ/ack
	TypeAudioEvent   Type = 127 // audioclient: событие плеера (mediad → platformd)
	TypeIOHubMisc    Type = 183 // iohub_services: служебное
	TypeHandshake    Type = 207 // aliced: возможности устройства при подключении
	TypeRemoting     Type = 210 // aliced: вызов метода капабилити
	TypeMetrica      Type = 24  // metricad
	TypeNetStatsReq  Type = 241
	TypeNetStats     Type = 240
	TypeAltURL       Type = 247 // audioclient: смена запасного URL
	TypeHealth       Type = 250 // update_health_check
)

func (t Type) String() string {
	switch t {
	case TypeAliceState:
		return "AliceState"
	case TypeAudioCommand:
		return "AudioCommand"
	case TypeIOHub:
		return "IOHub"
	case TypeIOHubReply:
		return "IOHubReply"
	case TypeAudioEvent:
		return "AudioEvent"
	case TypeIOHubMisc:
		return "IOHubMisc"
	case TypeHandshake:
		return "Handshake"
	case TypeRemoting:
		return "Remoting"
	case TypeMetrica:
		return "Metrica"
	case TypeNetStatsReq:
		return "NetStatsReq"
	case TypeNetStats:
		return "NetStats"
	case TypeAltURL:
		return "AltURL"
	case TypeHealth:
		return "Health"
	}
	return fmt.Sprintf("type%d", uint64(t))
}

var (
	ErrFrameSize  = errors.New("wire: недопустимая длина кадра")
	ErrNoEnvelope = errors.New("wire: нет конверта 252/253")
)

// FrameReader читает кадры из потока, переиспользуя один буфер.
type FrameReader struct {
	r   io.Reader
	buf []byte
	hdr [4]byte
}

func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

// Next возвращает следующий кадр. Срез действителен до следующего вызова.
func (fr *FrameReader) Next() ([]byte, error) {
	if cap(fr.buf) > keepBuf {
		fr.buf = nil
	}
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(fr.hdr[:])
	if n == 0 || n > MaxFrame {
		return nil, fmt.Errorf("%w: %d", ErrFrameSize, n)
	}
	if uint32(cap(fr.buf)) < n {
		fr.buf = make([]byte, n)
	}
	fr.buf = fr.buf[:n]
	if _, err := io.ReadFull(fr.r, fr.buf); err != nil {
		return nil, err
	}
	return fr.buf, nil
}

// AppendFrame дописывает к dst заголовок длины и сообщение.
func AppendFrame(dst, msg []byte) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], uint32(len(msg)))
	dst = append(dst, h[:]...)
	return append(dst, msg...)
}

// WriteFrame пишет один кадр одним вызовом Write.
func WriteFrame(w io.Writer, msg []byte) error {
	_, err := w.Write(AppendFrame(make([]byte, 0, 4+len(msg)), msg))
	return err
}

// Msg — распакованный кадр.
type Msg struct {
	Type Type
	Body []byte // payload (поле 252), ссылается на буфер кадра
	Raw  []byte // весь кадр целиком (поверх конверта бывают ещё поля, например у metricad)
}

// Unwrap достаёт тип и payload из конверта.
func Unwrap(frame []byte) (Msg, error) {
	m := Msg{Raw: frame}
	var haveType, haveBody bool
	r := pb.NewReader(frame)
	for {
		f, err := r.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return m, err
		}
		switch {
		case f.Num == 252 && f.Wire == pb.WireBytes:
			m.Body, haveBody = f.B, true
		case f.Num == 253 && f.Wire == pb.WireVarint:
			m.Type, haveType = Type(f.U), true
		}
	}
	if !haveType || !haveBody {
		return m, ErrNoEnvelope
	}
	return m, nil
}

// Wrap собирает конверт: поле 252 = тело, поле 253 = тип (именно в таком порядке, как в трафике).
func Wrap(t Type, body []byte) []byte {
	out := make([]byte, 0, len(body)+16)
	out = pb.AppendBytes(out, 252, body)
	return pb.AppendUint(out, 253, uint64(t))
}

// Remoting собирает вызов метода капабилити: {7: объект, method: тело} в конверте типа 210.
func Remoting(object string, method uint64, body []byte) []byte {
	var b []byte
	b = pb.AppendString(b, 7, object)
	b = pb.AppendBytes(b, method, body)
	return Wrap(TypeRemoting, b)
}

// CancelDialog — отмена текущего диалога Алисы (AliceCapabilityMethod, вид 3; та же команда, что 'c' в maind --cli).
func CancelDialog() []byte {
	return Remoting("AliceCapability", 8, pb.AppendUint(nil, 1, 3))
}

// PausePlayback — пауза штатного плеера (PlaybackControlCapability, вид 1; 'o' в maind --cli).
// id — произвольный UUID запроса, source — метка источника (в логе прошивки видна как «Keyboard»).
func PausePlayback(id, source string) []byte {
	var src []byte
	src = pb.AppendUint(src, 1, 14)
	src = pb.AppendUint(src, 2, 3)
	src = pb.AppendUint(src, 3, 1)
	src = pb.AppendString(src, 4, id)
	src = pb.AppendString(src, 6, source)
	var m []byte
	m = pb.AppendUint(m, 1, 1)
	m = pb.AppendBytes(m, 5, src)
	m = pb.AppendUint(m, 7, 0)
	return Remoting("PlaybackControlCapability", 13, m)
}
