package obs

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

const uuid = "4a096101-0114-46eb-b935-d27be0eb3ba1"

func collect() (func([]byte), *[][]byte) {
	var got [][]byte
	return func(b []byte) { got = append(got, append([]byte(nil), b...)) }, &got
}

func TestFlowSplitSegments(t *testing.T) {
	a, b := wire.CancelDialog(), wire.PausePlayback(uuid, "Keyboard")
	stream := wire.AppendFrame(wire.AppendFrame(nil, a), b)
	emit, got := collect()

	f := &flow{}
	f.add(1000, 0x02, nil, emit) // SYN
	seq := uint32(1001)
	for _, cut := range []int{3, 10, 30, len(stream)} { // режем посреди заголовка и посреди кадров
		start := int(seq - 1001)
		f.add(seq, 0x18, stream[start:cut], emit)
		seq = 1001 + uint32(cut)
	}
	if len(*got) != 2 || !bytes.Equal((*got)[0], a) || !bytes.Equal((*got)[1], b) {
		t.Fatalf("получили %d кадров", len(*got))
	}
}

func TestFlowRetransmit(t *testing.T) {
	a := wire.CancelDialog()
	frame := wire.AppendFrame(nil, a)
	emit, got := collect()
	f := &flow{}
	f.add(1000, 0x02, nil, emit)
	f.add(1001, 0x18, frame[:10], emit)
	f.add(1001, 0x18, frame[:10], emit) // полный повтор
	f.add(1001, 0x18, frame[:20], emit) // частичный повтор с продолжением
	f.add(1021, 0x18, frame[20:], emit)
	if len(*got) != 1 || !bytes.Equal((*got)[0], a) {
		t.Fatalf("получили %d кадров", len(*got))
	}
}

func TestFlowResyncAfterLoss(t *testing.T) {
	a, b := wire.CancelDialog(), wire.PausePlayback(uuid, "Keyboard")
	fa, fb := wire.AppendFrame(nil, a), wire.AppendFrame(nil, b)
	emit, got := collect()
	f := &flow{}
	f.add(1000, 0x02, nil, emit)
	f.add(1001, 0x18, fa[:5], emit) // кадр a не дошёл целиком
	// дыра в потоке: следующий сегмент начинается с границы кадра b
	f.add(1001+uint32(len(fa))+7, 0x18, fb, emit)
	if len(*got) != 1 || !bytes.Equal((*got)[0], b) {
		t.Fatalf("после дыры должен разобраться кадр b, получили %d кадров", len(*got))
	}
}

func TestFlowGarbageDoesNotSync(t *testing.T) {
	emit, got := collect()
	f := &flow{}
	f.add(5, 0x18, []byte{0x10, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, emit)
	if len(*got) != 0 {
		t.Fatal("мусор не должен давать кадров")
	}
}

func TestDescribe(t *testing.T) {
	voice := pb.AppendBytes(nil, 22, pb.AppendUint(pb.AppendString(pb.AppendUint(nil, 1, 2), 2, "72daa5dc-1111"), 4, 0))
	chunk := wire.Wrap(wire.TypeAudioCommand, append(voice, pb.AppendBytes(nil, 29, make([]byte, 8137))...))
	m, err := wire.Unwrap(chunk)
	if err != nil {
		t.Fatal(err)
	}
	line, show := Describe("audioclient", m, Opt{})
	if !show || !strings.Contains(line, "voice-chunk") || !strings.Contains(line, "8137B") || !strings.Contains(line, "voice/72daa5dc") {
		t.Errorf("%q show=%v", line, show)
	}

	// прогресс плеера по умолчанию скрыт, с -progress виден
	ev := wire.Wrap(wire.TypeAudioEvent, append(pb.AppendUint(nil, 1, 2), pb.AppendUint(nil, 2, 3)...))
	m, _ = wire.Unwrap(ev)
	if _, show := Describe("audioclient", m, Opt{}); show {
		t.Error("прогресс должен быть скрыт")
	}
	if line, show := Describe("audioclient", m, Opt{Progress: true}); !show || !strings.Contains(line, "progress PLAYING") {
		t.Errorf("%q %v", line, show)
	}

	// фон: IOHub скрыт без -noisy
	m, _ = wire.Unwrap(wire.Wrap(wire.TypeIOHub, pb.AppendUint(nil, 1, 1)))
	if _, show := Describe("iohub_clients", m, Opt{}); show {
		t.Error("iohub должен быть скрыт")
	}
	if line, show := Describe("iohub_clients", m, Opt{Noisy: true}); !show || !strings.HasPrefix(line, "IOHub") {
		t.Errorf("%q %v", line, show)
	}
}

func TestDescribeHidesURLTokens(t *testing.T) {
	secret := "ysign1=deadbeefdeadbeef"
	url := "https://strm-m9-75.strm.yandex.net/music-v2/crypt/" + secret + "/x"
	track := pb.AppendString(nil, 1, "44683635")
	track = pb.AppendString(track, 2, url)
	body := pb.AppendBytes(nil, 1, track)
	body = pb.AppendBytes(body, 22, pb.AppendString(pb.AppendUint(nil, 1, 1), 2, "76006c91-x"))
	m, _ := wire.Unwrap(wire.Wrap(wire.TypeAudioCommand, body))
	line, _ := Describe("audioclient", m, Opt{})
	if strings.Contains(line, "ysign") || !strings.Contains(line, "strm-m9-75.strm.yandex.net") {
		t.Errorf("в строке должен остаться только хост: %q", line)
	}
}

func TestDescribeAliceReply(t *testing.T) {
	reply := pb.AppendString(nil, 1, "Включаю.")
	reply = pb.AppendBytes(reply, 5, []byte(`{"response":{"directives":[{"name":"tts_play_placeholder"},{"name":"audio_play"}]}}`))
	s := pb.AppendUint(nil, 1, 3)
	s = pb.AppendString(s, 2, "Включи музыку")
	s = pb.AppendBytes(s, 3, reply)
	m, _ := wire.Unwrap(wire.Wrap(wire.TypeAliceState, s))
	line, _ := Describe("aliced", m, Opt{})
	for _, want := range []string{"BUSY", `text="Включи музыку"`, `reply="Включаю."`, "[tts_play_placeholder audio_play]"} {
		if !strings.Contains(line, want) {
			t.Errorf("нет %q в %q", want, line)
		}
	}
}

// pcapPacket собирает одну запись classic-pcap: Ethernet + IPv4 + TCP с payload.
func pcapPacket(sport, dport uint16, seq uint32, flags byte, payload []byte) []byte {
	pkt := make([]byte, 14+20+20, 14+20+20+len(payload))
	binary.BigEndian.PutUint16(pkt[12:], 0x0800)
	ip := pkt[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+20+len(payload)))
	ip[9] = 6
	copy(ip[12:], []byte{127, 0, 0, 1})
	copy(ip[16:], []byte{127, 0, 0, 1})
	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[0:], sport)
	binary.BigEndian.PutUint16(tcp[2:], dport)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	tcp[12] = 5 << 4
	tcp[13] = flags
	pkt = append(pkt, payload...)

	rec := make([]byte, 16)
	binary.LittleEndian.PutUint32(rec[0:], 1793049083)
	binary.LittleEndian.PutUint32(rec[4:], 123000)
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(pkt)))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(pkt)))
	return append(rec, pkt...)
}

func TestDecodePcapSynthetic(t *testing.T) {
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(hdr[4:], 2)
	binary.LittleEndian.PutUint16(hdr[6:], 4)
	binary.LittleEndian.PutUint32(hdr[16:], 65535)
	binary.LittleEndian.PutUint32(hdr[20:], 1)

	state := pb.AppendUint(nil, 1, 1)
	state = pb.AppendString(state, 2, "Пауза")
	frame := wire.AppendFrame(nil, wire.Wrap(wire.TypeAliceState, state))
	cancel := wire.AppendFrame(nil, wire.CancelDialog())

	cap := append([]byte(nil), hdr...)
	cap = append(cap, pcapPacket(9875, 40000, 100, 0x02, nil)...) // SYN сервера
	cap = append(cap, pcapPacket(9875, 40000, 101, 0x18, frame)...)
	cap = append(cap, pcapPacket(40000, 9875, 7, 0x18, cancel)...) // клиент → сервер, без SYN
	cap = append(cap, pcapPacket(40001, 40002, 1, 0x18, frame)...) // не наш порт — игнорируется

	var out bytes.Buffer
	if err := DecodePcap(bytes.NewReader(cap), Opt{}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "S→C AliceState LISTENING") || !strings.Contains(s, `text="Пауза"`) {
		t.Errorf("нет ответа сервера:\n%s", s)
	}
	if !strings.Contains(s, "C→S Remoting AliceCapability methods=[8]") {
		t.Errorf("нет команды клиента:\n%s", s)
	}
	if n := strings.Count(s, "\n"); n != 2 {
		t.Errorf("ждали 2 строки, получили %d:\n%s", n, s)
	}
}

func TestDecodePcapRejectsPcapng(t *testing.T) {
	if err := DecodePcap(bytes.NewReader(append([]byte{0x0a, 0x0d, 0x0d, 0x0a}, make([]byte, 40)...)), Opt{}, &bytes.Buffer{}); err == nil {
		t.Error("pcapng должен отвергаться понятной ошибкой")
	}
}

// Реплей настоящей записи шины: OMD_PCAP=путь\к\music.pcap go test ./internal/obs -run Real -v
// Запись содержит идентификаторы аккаунта, поэтому в репозитории её нет — тест пропускается без переменной.
func TestRealCapture(t *testing.T) {
	path := os.Getenv("OMD_PCAP")
	if path == "" {
		t.Skip("OMD_PCAP не задан")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	if err := DecodePcap(f, Opt{Progress: true}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"AliceState LISTENING", "final", "directives=[tts_play_placeholder audio_play",
		"AudioCommand voice-start", "AudioCommand voice-chunk", "AudioCommand voice-eos", "AudioCommand release",
		"AudioCommand play", "AudioCommand prefetch", "AudioCommand pause",
		"AudioEvent progress PLAYING", "AudioEvent state ENDED", "AudioEvent state PAUSED",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("в реальной записи не найдено %q", want)
		}
	}
	t.Logf("строк в разборе: %d", strings.Count(s, "\n"))
}
