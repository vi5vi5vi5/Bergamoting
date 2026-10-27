// Package shim — прозрачный посредник между клиентами и сервисом шины Quasar (фаза 2 плана).
//
// Каждое входящее соединение получает собственное исходящее к настоящему сервису, так что рассылки сервиса
// «всем клиентам» и порядок кадров сохраняются. Кадры [u32 LE длина][сообщение] пересылаются байт в байт:
// шим ничего не разбирает для себя и ничего не меняет, а Tap только наблюдает уже отправленное. Если поток
// перестал быть похож на кадры шины, оставшиеся байты копируются как есть, а не обрываются.
package shim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"openmediad/internal/wire"
)

// Dir — направление кадра.
type Dir uint8

const (
	ToService Dir = iota // клиент → сервис
	ToClient             // сервис → клиент
)

func (d Dir) String() string {
	if d == ToService {
		return "→сервис"
	}
	return "→клиент"
}

// Conn — одно проксируемое соединение.
type Conn struct {
	ID     uint64
	Client string // адрес клиента, как его видит шим
	Opened time.Time

	frames [2]atomic.Uint64
	bytes  [2]atomic.Uint64
	raw    [2]atomic.Uint64 // байты, пересланные без разбора на кадры
	eaten  atomic.Uint64    // кадры клиента, обработанные перехватчиком и не пересланные сервису

	wmu     sync.Mutex // записи клиенту: кадры сервиса и впрыснутые шимом не должны перемешиваться
	umu     sync.Mutex // записи сервису: кадры клиента и пересылка по просьбе перехватчика
	cli, up *net.TCPConn
}

// lockedWriter сериализует записи в одно соединение: кадр уходит целиком или не уходит.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// Tap вызывается после того, как кадр целиком записан получателю. frame — сообщение без заголовка длины,
// срез действителен только внутри вызова. lat — время от прихода начала кадра до конца записи получателю.
type Tap func(c *Conn, d Dir, frame []byte, lat time.Duration)

type Proxy struct {
	Listen   string // например 127.0.0.1:19906 (для Run)
	Upstream string // настоящий сервис, например 127.0.0.1:9906
	Src      net.IP // локальный адрес исходящих соединений: по нему правило NAT отличает шим от клиентов
	Tap      Tap    // необязательный наблюдатель
	Logf     func(string, ...any)
	Grace    time.Duration // сколько ждать вторую половину после закрытия одной стороны (по умолчанию 5 с)

	// Intercept (необязательный) видит каждый кадр клиента перед отправкой сервису. true — кадр обработан
	// перехватчиком и сервису не отправляется. Вызывается из горутины соединения: должен возвращаться быстро.
	// frame действителен только внутри вызова.
	Intercept func(c *Conn, frame []byte) bool

	nextID atomic.Uint64

	wg sync.WaitGroup // живые соединения

	mu          sync.Mutex // защищает поля ниже
	conns       map[uint64]*Conn
	lastRefused time.Time
	skipped     int
	gate        func() // закрывает слушатель Run при отказе сервиса; nil у Serve
}

func (p *Proxy) logf(format string, a ...any) {
	if p.Logf != nil {
		p.Logf(format, a...)
	}
}

// Run слушает p.Listen, пока сервис отвечает. Как только сервис отказал в соединении (например, mediad
// перезапускается), слушатель закрывается и клиенты получают отказ от ядра, как у настоящего сервиса; когда сервис
// снова принимает соединения, слушатель открывается заново. Без этого клиенты видели бы «подключились и сразу
// оборвали», а не «отказано», и засыпали бы журнал платформы предупреждениями.
func (p *Proxy) Run(ctx context.Context) error {
	defer p.wg.Wait()
	for {
		if !p.waitService(ctx) {
			return nil
		}
		ln, err := net.Listen("tcp", p.Listen)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.gate = func() { ln.Close() }
		p.mu.Unlock()
		p.logf("сервис %s отвечает, слушаю %s", p.Upstream, p.Listen)
		err = p.accept(ctx, ln)
		p.mu.Lock()
		p.gate = nil
		p.mu.Unlock()
		if err != nil || ctx.Err() != nil {
			return err
		}
		p.logf("сервис недоступен: слушатель закрыт, клиентам отказывает ядро")
	}
}

// waitService возвращается, когда сервис принимает соединения (false — отменён ctx).
func (p *Proxy) waitService(ctx context.Context) bool {
	for {
		d := net.Dialer{Timeout: time.Second}
		if p.Src != nil {
			d.LocalAddr = &net.TCPAddr{IP: p.Src}
		}
		if c, err := d.DialContext(ctx, "tcp", p.Upstream); err == nil {
			c.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Serve принимает соединения с готового слушателя (он не закрывается при отказе сервиса, в отличие от Run)
// и возвращается, когда слушатель закрыт или отменён ctx и все соединения завершены.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	err := p.accept(ctx, ln)
	p.wg.Wait()
	return err
}

// accept принимает соединения, пока слушатель не закрыт; закрытие (отменой ctx или самим шимом) — штатный выход.
func (p *Proxy) accept(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		cli, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.serve(ctx, cli.(*net.TCPConn))
		}()
	}
}

func (p *Proxy) serve(ctx context.Context, cli *net.TCPConn) {
	defer cli.Close()
	d := net.Dialer{Timeout: 2 * time.Second}
	if p.Src != nil {
		d.LocalAddr = &net.TCPAddr{IP: p.Src}
	}
	raw, err := d.DialContext(ctx, "tcp", p.Upstream)
	if err != nil {
		// Пока сервис перезапускается, прямой клиент получил бы отказ в соединении. Лучшее, что можно сделать
		// из принятого сокета, — сразу оборвать его RST, а не вежливым FIN.
		cli.SetLinger(0)
		p.refused(cli.RemoteAddr(), err)
		p.mu.Lock()
		gate := p.gate
		p.mu.Unlock()
		if gate != nil {
			gate()
		}
		return
	}
	up := raw.(*net.TCPConn)
	defer up.Close()

	c := &Conn{ID: p.nextID.Add(1), Client: cli.RemoteAddr().String(), Opened: time.Now(), cli: cli, up: up}
	p.mu.Lock()
	if p.conns == nil {
		p.conns = map[uint64]*Conn{}
	}
	p.conns[c.ID] = c
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, c.ID)
		p.mu.Unlock()
	}()
	p.logf("conn %d: клиент %s ↔ %s открыто", c.ID, c.Client, p.Upstream)

	grace := p.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	// Закрыть оба конца при отмене: иначе остановка шима зависла бы на живых соединениях.
	stop := context.AfterFunc(ctx, func() { cli.Close(); up.Close() })
	defer stop()

	var wg sync.WaitGroup
	run := func(d Dir, src, dst *net.TCPConn, w io.Writer) {
		defer wg.Done()
		err := p.pump(c, d, src, w)
		if err == nil {
			// Источник закрылся штатно: передаём FIN дальше, а вторую половину ждём ограниченное время.
			dst.CloseWrite()
			dst.SetReadDeadline(time.Now().Add(grace))
			return
		}
		if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
			p.logf("conn %d %s: %v", c.ID, d, err)
		}
		src.Close()
		dst.Close()
	}
	wg.Add(2)
	go run(ToService, cli, up, &lockedWriter{&c.umu, up})
	go run(ToClient, up, cli, &lockedWriter{&c.wmu, cli})
	wg.Wait()
	p.logf("conn %d: закрыто через %s; кадров →сервис %d (%d Б), →клиент %d (%d Б); без разбора %d/%d Б",
		c.ID, time.Since(c.Opened).Round(time.Millisecond),
		c.frames[ToService].Load(), c.bytes[ToService].Load(),
		c.frames[ToClient].Load(), c.bytes[ToClient].Load(),
		c.raw[ToService].Load(), c.raw[ToClient].Load())
}

// refused пишет в журнал об отказе сервиса не чаще раза в секунду: пока сервис перезапускается, клиенты
// пробуют подключиться сотнями раз в секунду, и построчный журнал забил бы память (/tmp на колонке — tmpfs).
func (p *Proxy) refused(client net.Addr, err error) {
	p.mu.Lock()
	now := time.Now()
	if now.Sub(p.lastRefused) < time.Second {
		p.skipped++
		p.mu.Unlock()
		return
	}
	n := p.skipped
	p.skipped, p.lastRefused = 0, now
	p.mu.Unlock()
	more := ""
	if n > 0 {
		more = fmt.Sprintf(" (ещё %d таких же за последнюю секунду)", n)
	}
	p.logf("клиент %s: сервис недоступен (%v), соединение оборвано%s", client, err, more)
}

// keepBuf — буфер крупнее этого отпускается после большого кадра (как в wire.FrameReader).
const keepBuf = 64 << 10

// pump пересылает кадры из src в dst. nil означает, что src закрылся штатно (EOF).
func (p *Proxy) pump(c *Conn, d Dir, src io.Reader, dst io.Writer) error {
	var buf []byte
	for {
		if cap(buf) > keepBuf {
			buf = nil
		}
		if cap(buf) < 4096 {
			buf = make([]byte, 4096)
		}
		n, err := io.ReadFull(src, buf[:4])
		if err != nil {
			return flushPartial(dst, buf[:n], err)
		}
		t0 := time.Now()
		size := binary.LittleEndian.Uint32(buf[:4])
		if size == 0 || size > wire.MaxFrame {
			p.logf("conn %d %s: заголовок %d не похож на кадр шины, дальше пересылаю байты как есть", c.ID, d, size)
			if _, err := dst.Write(buf[:4]); err != nil {
				return err
			}
			c.raw[d].Add(4)
			m, err := io.Copy(dst, src)
			c.raw[d].Add(uint64(m))
			return err
		}
		total := 4 + int(size)
		if cap(buf) < total {
			nb := make([]byte, total)
			copy(nb, buf[:4])
			buf = nb
		}
		n, err = io.ReadFull(src, buf[4:total])
		if err != nil {
			return flushPartial(dst, buf[:4+n], err)
		}
		if d == ToService && p.Intercept != nil && p.Intercept(c, buf[4:total]) {
			c.eaten.Add(1)
			continue
		}
		if _, err := dst.Write(buf[:total]); err != nil {
			return err
		}
		lat := time.Since(t0)
		c.frames[d].Add(1)
		c.bytes[d].Add(uint64(total))
		if p.Tap != nil {
			p.Tap(c, d, buf[4:total], lat)
		}
	}
}

// flushPartial досылает то, что успели прочитать до конца потока: обрыв посреди кадра не должен
// выглядеть для получателя иначе, чем при прямом соединении.
func flushPartial(dst io.Writer, got []byte, readErr error) error {
	if len(got) > 0 {
		if _, err := dst.Write(got); err != nil {
			return err
		}
	}
	if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
		return nil
	}
	return fmt.Errorf("чтение: %w", readErr)
}

// Broadcast отправляет сообщение (конверт без заголовка длины) всем клиентам, как рассылка сервиса. Возвращает,
// скольким клиентам кадр ушёл. Медленному клиенту даётся 2 с, потом он пропускается.
func (p *Proxy) Broadcast(msg []byte) int {
	frame := wire.AppendFrame(make([]byte, 0, 4+len(msg)), msg)
	p.mu.Lock()
	conns := make([]*Conn, 0, len(p.conns))
	for _, c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	sent := 0
	for _, c := range conns {
		c.wmu.Lock()
		c.cli.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err := c.cli.Write(frame)
		c.cli.SetWriteDeadline(time.Time{})
		c.wmu.Unlock()
		if err != nil {
			p.logf("conn %d: рассылка: %v", c.ID, err)
			continue
		}
		sent++
	}
	return sent
}

// ForwardTo отправляет сообщение сервису от имени соединения c (так перехватчик возвращает команду настоящему сервису).
func (p *Proxy) ForwardTo(c *Conn, msg []byte) error {
	frame := wire.AppendFrame(make([]byte, 0, 4+len(msg)), msg)
	c.umu.Lock()
	defer c.umu.Unlock()
	_, err := c.up.Write(frame)
	return err
}

// NumConns — сколько клиентских соединений сейчас проксируется.
func (p *Proxy) NumConns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}
