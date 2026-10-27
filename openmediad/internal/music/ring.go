package music

import (
	"errors"
	"io"
	"sync"
)

var (
	errWake    = errors.New("music: чтение прервано (пауза/смена)")
	errStopped = errors.New("music: буфер остановлен")
)

// ring — ограниченный буфер PCM между декодером и выводом. Запись блокируется, пока буфер полон: так давление
// от медленного вывода доходит до декодера и скачивания, а предзагруженный трек не съедает память.
type ring struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	r, n    int
	closed  bool // писатель закончил: после опустошения чтение вернёт io.EOF
	stopped bool // буфер брошен: писатель и читатель получают errStopped
	wake    bool
}

func newRing(size int) *ring {
	r := &ring{buf: make([]byte, size)}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// Write копирует p в буфер, блокируясь при нехватке места.
func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for len(p) > 0 {
		for r.n == len(r.buf) && !r.stopped {
			r.cond.Wait()
		}
		if r.stopped {
			return total, errStopped
		}
		w := (r.r + r.n) % len(r.buf)
		k := min(len(p), len(r.buf)-r.n, len(r.buf)-w)
		copy(r.buf[w:], p[:k])
		r.n += k
		p = p[k:]
		total += k
		r.cond.Broadcast()
	}
	return total, nil
}

// ReadWait читает не больше len(p) байт. Блокируется, пока нет данных; возвращает errWake после Wake,
// io.EOF, когда писатель закончил и буфер пуст, и errStopped после Stop.
func (r *ring) ReadWait(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.n == 0 && !r.closed && !r.stopped && !r.wake {
		r.cond.Wait()
	}
	if r.wake {
		r.wake = false
		return 0, errWake
	}
	if r.stopped {
		return 0, errStopped
	}
	if r.n == 0 {
		return 0, io.EOF
	}
	k := min(len(p), r.n, len(r.buf)-r.r)
	copy(p, r.buf[r.r:r.r+k])
	r.r = (r.r + k) % len(r.buf)
	r.n -= k
	r.cond.Broadcast()
	return k, nil
}

// WaitLevel ждёт, пока в буфере будет не меньше want байт или писатель закончит. false — буфер брошен.
func (r *ring) WaitLevel(want int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.n < want && !r.closed && !r.stopped {
		r.cond.Wait()
	}
	return !r.stopped
}

func (r *ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// CloseWrite сообщает, что данных больше не будет.
func (r *ring) CloseWrite() {
	r.mu.Lock()
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

// Stop бросает буфер: все ожидающие просыпаются с ошибкой.
func (r *ring) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

// Wake будит одно ожидающее чтение (оно вернёт errWake), не теряя данных.
func (r *ring) Wake() {
	r.mu.Lock()
	r.wake = true
	r.cond.Broadcast()
	r.mu.Unlock()
}
