package obs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

func TestDecodePcapSavesVoiceChunks(t *testing.T) {
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint32(hdr[20:], 1)

	ogg := append([]byte("OggS"), bytes.Repeat([]byte{7}, 100)...)
	body := pb.AppendBytes(nil, 22, pb.AppendString(pb.AppendUint(nil, 1, 2), 2, "72daa5dc-1111"))
	body = pb.AppendBytes(body, 29, ogg)
	frame := wire.AppendFrame(nil, wire.Wrap(wire.TypeAudioCommand, body))

	cap := append([]byte(nil), hdr...)
	cap = append(cap, pcapPacket(40000, 9906, 1, 0x18, frame)...)

	dir := t.TempDir()
	var out bytes.Buffer
	if err := DecodePcap(bytes.NewReader(cap), Opt{VoiceDir: dir}, &out); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.ogg"))
	if len(files) != 1 {
		t.Fatalf("ждали 1 файл, получили %d", len(files))
	}
	got, _ := os.ReadFile(files[0])
	if !bytes.Equal(got, ogg) {
		t.Error("содержимое чанка искажено")
	}
	if name := filepath.Base(files[0]); !strings.HasSuffix(name, "-72daa5dc.ogg") {
		t.Errorf("имя файла: %s", name)
	}
}
