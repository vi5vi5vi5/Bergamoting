package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"openmediad/internal/pb"
)

// Эталонные кадры сняты с живой колонки командой maind --cli (клавиши 'c' и 'o'), прошивка .117.
const (
	realCancelDialog = "e20f153a0f416c6963654361706162696c69747942020803e80fd201"
	realPause        = "e20f593a19506c61796261636b436f6e74726f6c4361706162696c6974796a3c08012a36080e10031801222434613039363130312d303131342d343665622d623933352d64323762653065623362613132084b6579626f6172643800e80fd201"
	realPauseID      = "4a096101-0114-46eb-b935-d27be0eb3ba1"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCancelDialogMatchesRealTraffic(t *testing.T) {
	if got, want := CancelDialog(), unhex(t, realCancelDialog); !bytes.Equal(got, want) {
		t.Fatalf("CancelDialog:\n получили %x\n ждали    %x", got, want)
	}
}

func TestPausePlaybackMatchesRealTraffic(t *testing.T) {
	if got, want := PausePlayback(realPauseID, "Keyboard"), unhex(t, realPause); !bytes.Equal(got, want) {
		t.Fatalf("PausePlayback:\n получили %x\n ждали    %x", got, want)
	}
}

func TestUnwrapRealFrames(t *testing.T) {
	m, err := Unwrap(unhex(t, realCancelDialog))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != TypeRemoting {
		t.Fatalf("тип %v", m.Type)
	}
	if obj, _ := pb.Str(m.Body, 7); obj != "AliceCapability" {
		t.Fatalf("объект %q", obj)
	}
	method, _ := pb.Bytes(m.Body, 8)
	if kind, _ := pb.Uint(method, 1); kind != 3 {
		t.Fatalf("вид метода %d, ждали 3 (CANCEL_DIALOG)", kind)
	}

	m, err = Unwrap(unhex(t, realPause))
	if err != nil {
		t.Fatal(err)
	}
	method, _ = pb.Bytes(m.Body, 13)
	src, _ := pb.Bytes(method, 5)
	if id, _ := pb.Str(src, 4); id != realPauseID {
		t.Fatalf("uuid %q", id)
	}
	if s, _ := pb.Str(src, 6); s != "Keyboard" {
		t.Fatalf("source %q", s)
	}
}

func TestWrapUnwrap(t *testing.T) {
	body := pb.AppendString(nil, 1, "x")
	m, err := Unwrap(Wrap(TypeAliceState, body))
	if err != nil || m.Type != TypeAliceState || !bytes.Equal(m.Body, body) {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestUnwrapExtraFieldsAreKept(t *testing.T) {
	// metricad кладёт рядом с конвертом ещё поле 1 (UUID события).
	frame := append(Wrap(TypeMetrica, []byte{0x0a, 0x00}), pb.AppendString(nil, 1, "ev-1")...)
	m, err := Unwrap(frame)
	if err != nil || m.Type != TypeMetrica {
		t.Fatalf("%+v %v", m, err)
	}
	if s, _ := pb.Str(m.Raw, 1); s != "ev-1" {
		t.Fatalf("дополнительное поле потеряно: %q", s)
	}
}

func TestUnwrapErrors(t *testing.T) {
	if _, err := Unwrap(pb.AppendString(nil, 252, "тело")); !errors.Is(err, ErrNoEnvelope) {
		t.Errorf("нет типа: %v", err)
	}
	if _, err := Unwrap(pb.AppendUint(nil, 253, 3)); !errors.Is(err, ErrNoEnvelope) {
		t.Errorf("нет тела: %v", err)
	}
	if _, err := Unwrap([]byte{0x0a, 0x05, 'a'}); !errors.Is(err, pb.ErrTruncated) {
		t.Errorf("битый кадр: %v", err)
	}
}

func TestFrameReader(t *testing.T) {
	a, b := CancelDialog(), PausePlayback(realPauseID, "x")
	stream := AppendFrame(AppendFrame(nil, a), b)
	fr := NewFrameReader(bytes.NewReader(stream))
	for i, want := range [][]byte{a, b} {
		got, err := fr.Next()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("кадр %d: %v", i, err)
		}
	}
	if _, err := fr.Next(); err != io.EOF {
		t.Fatalf("ждали io.EOF, получили %v", err)
	}
}

func TestFrameReaderRejectsBadLengths(t *testing.T) {
	var h [4]byte
	for name, n := range map[string]uint32{"нулевая": 0, "больше лимита": MaxFrame + 1} {
		binary.LittleEndian.PutUint32(h[:], n)
		if _, err := NewFrameReader(bytes.NewReader(h[:])).Next(); !errors.Is(err, ErrFrameSize) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// обрыв посреди кадра
	binary.LittleEndian.PutUint32(h[:], 10)
	if _, err := NewFrameReader(bytes.NewReader(append(h[:], 1, 2, 3))).Next(); err != io.ErrUnexpectedEOF {
		t.Errorf("обрыв: %v", err)
	}
}

func TestFrameReaderDropsBigBuffer(t *testing.T) {
	big := bytes.Repeat([]byte{0x20}, keepBuf*2) // 0x20 — не валидный protobuf, но FrameReader это не проверяет
	fr := NewFrameReader(bytes.NewReader(AppendFrame(AppendFrame(nil, big), []byte{1})))
	if _, err := fr.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := fr.Next(); err != nil {
		t.Fatal(err)
	}
	if cap(fr.buf) > keepBuf {
		t.Errorf("крупный буфер должен отпускаться, cap=%d", cap(fr.buf))
	}
}

func TestTypeString(t *testing.T) {
	if TypeAudioEvent.String() != "AudioEvent" || Type(999).String() != "type999" {
		t.Error("Type.String")
	}
}
