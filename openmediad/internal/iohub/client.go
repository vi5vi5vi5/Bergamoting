package iohub

import (
	"context"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"openmediad/internal/wire"
)

func f32bits(v float32) uint32 { return math.Float32bits(v) }

// Client отправляет сообщения сервису iohub по одному постоянному соединению. Соединение открывается при первой отправке
// и закрывается после Idle без отправок, чтобы шим без музыки не висел в iohub лишним клиентом. Всё, что сервис присылает,
// вычитывается и отбрасывается (иначе сервер упёрся бы в переполненный сокет). Send никогда не блокируется: пока
// соединения нет или очередь полна, сообщение пропадает — для спектра потеря одного кадра безвредна.
type Client struct {
	Addr string        // по умолчанию 127.0.0.1:9903
	Idle time.Duration // по умолчанию 5 минут
	Logf func(string, ...any)

	once sync.Once
	q    chan []byte
}

func (c *Client) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

// Run обслуживает очередь, пока не отменён ctx.
func (c *Client) Run(ctx context.Context) {
	c.once.Do(func() { c.q = make(chan []byte, 128) })
	addr, idle := c.Addr, c.Idle
	if addr == "" {
		addr = "127.0.0.1:9903"
	}
	if idle == 0 {
		idle = 5 * time.Minute
	}
	backoff := time.Second
	for ctx.Err() == nil {
		// ждём первого сообщения: без него соединение не нужно
		var first []byte
		select {
		case <-ctx.Done():
			return
		case first = <-c.q:
		}
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			c.logf("iohub %s: %v (повтор через %s)", addr, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		c.logf("iohub: подключён к %s", addr)
		c.serve(ctx, conn, first, idle)
		conn.Close()
	}
}

// serve пишет сообщения из очереди в соединение, пока оно живо и не наступил простой.
func (c *Client) serve(ctx context.Context, conn net.Conn, first []byte, idle time.Duration) {
	dead := make(chan struct{})
	go func() { // вычитываем всё, что присылает iohub
		io.Copy(io.Discard, conn)
		close(dead)
	}()
	write := func(msg []byte) bool {
		conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		_, err := conn.Write(wire.AppendFrame(make([]byte, 0, 4+len(msg)), msg))
		if err != nil {
			c.logf("iohub: запись: %v", err)
		}
		return err == nil
	}
	if !write(first) {
		return
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-dead:
			c.logf("iohub: соединение закрыто сервисом")
			return
		case <-timer.C:
			c.logf("iohub: %s без отправок, соединение закрыто", idle)
			return
		case msg := <-c.q:
			if !write(msg) {
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		}
	}
}

// Send ставит сообщение в очередь (конверт без заголовка длины). Возвращает false, если оно отброшено.
func (c *Client) Send(msg []byte) bool {
	c.once.Do(func() { c.q = make(chan []byte, 128) })
	select {
	case c.q <- msg:
		return true
	default:
		return false
	}
}
