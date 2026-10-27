//go:build linux

package music

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type gstDecoder struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *os.File
	done chan struct{}
	once sync.Once
	tail tailBuf
}

// NewGstDecoder запускает maind --gst-launch с конвейером декодирования AAC. Один процесс — одна сессия.
func NewGstDecoder() (Decoder, error) {
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	d := &gstDecoder{done: make(chan struct{}), out: rd}
	d.cmd = exec.Command(MaindPath, "--gst-launch", gstPipeline)
	d.cmd.Stdout = &d.tail
	d.cmd.Stderr = &d.tail
	d.cmd.ExtraFiles = []*os.File{wr} // станет fd 3 в дочернем процессе
	in, err := d.cmd.StdinPipe()
	if err != nil {
		rd.Close()
		wr.Close()
		return nil, err
	}
	d.in = in
	if err := d.cmd.Start(); err != nil {
		rd.Close()
		wr.Close()
		return nil, fmt.Errorf("запуск %s: %w", MaindPath, err)
	}
	wr.Close() // у нас остаётся только читающий конец; EOF придёт, когда процесс закроет свой
	go func() {
		d.cmd.Wait()
		close(d.done)
	}()
	return d, nil
}

func (d *gstDecoder) Write(p []byte) (int, error) { return d.in.Write(p) }
func (d *gstDecoder) Read(p []byte) (int, error)  { return d.out.Read(p) }
func (d *gstDecoder) CloseWrite() error           { return d.in.Close() }

// Close закрывает вход и просит процесс завершиться (SIGTERM); если он не слушается, добивает SIGKILL —
// это не aplay, оборванный GStreamer-декодер на общий смеситель звука не влияет.
func (d *gstDecoder) Close() error {
	d.once.Do(func() {
		d.in.Close()
		select {
		case <-d.done:
		case <-time.After(200 * time.Millisecond):
			d.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-d.done:
			case <-time.After(2 * time.Second):
				d.cmd.Process.Kill()
				<-d.done
			}
		}
		d.out.Close()
	})
	return nil
}

// Tail — хвост журнала процесса (для сообщения об ошибке).
func (d *gstDecoder) Tail() string { return d.tail.String() }

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

var _ = errors.New
