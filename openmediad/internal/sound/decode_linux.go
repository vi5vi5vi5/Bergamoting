//go:build linux

package sound

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// gstFile — конвейер раскодирования файла внутри maind --gst-launch (проверено на колонке: timer.wav, MP3 под именем .wav,
// даёт 1 780 504 байт ≈ 9,27 с за ≈0,5 с). PCM уходит в fd 3, потому что stdout и stderr заняты журналом maind.
const gstFile = "filesrc location=%s ! decodebin ! audioconvert ! audioresample ! " +
	"audio/x-raw,rate=48000,channels=2,format=S16LE ! fdsink fd=3 sync=false"

const (
	// maxPCM — предел раскодированного звука (≈87 с стерео): длинный файл в памяти колонки не держим, его играет mediad.
	maxPCM        = 16 << 20
	decodeTimeout = 20 * time.Second
)

// DecodeFile раскодирует файл в PCM S16LE 48 кГц стерео через GStreamer колонки.
func DecodeFile(path string) (*Clip, error) {
	if !ValidPath(path) {
		return nil, fmt.Errorf("недопустимый путь %q", path)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	var log tailBuf
	cmd := exec.Command(MaindPath, "--gst-launch", fmt.Sprintf(gstFile, path))
	cmd.Stdout, cmd.Stderr = &log, &log
	cmd.ExtraFiles = []*os.File{wr} // станет fd 3 в дочернем процессе
	if err := cmd.Start(); err != nil {
		wr.Close()
		return nil, fmt.Errorf("запуск %s: %w", MaindPath, err)
	}
	wr.Close() // у нас остаётся только читающий конец; EOF придёт, когда процесс закроет свой
	// Зависший или слишком длинный разбор прерываем: это GStreamer, а не aplay, SIGKILL на смеситель звука не влияет.
	timer := time.AfterFunc(decodeTimeout, func() { cmd.Process.Kill() })
	var buf bytes.Buffer
	buf.Grow(2 << 20)
	n, rerr := io.Copy(&buf, io.LimitReader(rd, maxPCM+1))
	timer.Stop()
	tooLong := n > maxPCM
	if rerr != nil || tooLong {
		cmd.Process.Kill()
	}
	werr := cmd.Wait()
	switch {
	case tooLong:
		return nil, fmt.Errorf("звук длиннее %d МБ", maxPCM>>20)
	case rerr != nil:
		return nil, rerr
	case werr != nil:
		return nil, fmt.Errorf("%s: %w: %s", MaindPath, werr, log.String())
	}
	pcm := buf.Bytes()
	pcm = pcm[:len(pcm)&^3] // целое число стереокадров
	return &Clip{PCM: pcm, Bitrate: FileBitrate(path)}, nil
}

// tailBuf хранит последние ~2 КБ вывода процесса.
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
