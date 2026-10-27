package shim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

const wait = 5 * time.Second

// rig — поддельный сервис, шим перед ним и функции подключения клиентов.
type rig struct {
	t        *testing.T
	upstream net.Listener
	proxy    net.Listener
	cancel   context.CancelFunc
	stopped  chan struct{} // закрывается, когда Serve вернулся
	err      error         // результат Serve; читать после <-stopped
}

func newRig(t *testing.T, tune func(*Proxy)) *rig {
	t.Helper()
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Upstream: up.Addr().String(), Grace: time.Second}
	if tune != nil {
		tune(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{t: t, upstream: up, proxy: pl, cancel: cancel, stopped: make(chan struct{})}
	go func() {
		r.err = p.Serve(ctx, pl)
		close(r.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		up.Close()
		select {
		case <-r.stopped:
		case <-time.After(wait):
			t.Error("шим не остановился после отмены")
		}
	})
	return r
}

// connect открывает клиента и принимает парное соединение на стороне сервиса.
func (r *rig) connect() (client, service *net.TCPConn) {
	r.t.Helper()
	c, err := net.Dial("tcp", r.proxy.Addr().String())
	if err != nil {
		r.t.Fatal(err)
	}
	r.upstream.(*net.TCPListener).SetDeadline(time.Now().Add(wait))
	s, err := r.upstream.Accept()
	if err != nil {
		r.t.Fatal(err)
	}
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

func frame(n int, seed int64) []byte {
	body := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(body)
	return wire.AppendFrame(nil, body)
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("чтение %d байт: %v", n, err)
	}
	return b
}

func expectEOF(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	if n, err := c.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("ждали конец потока, получили %d Б, err=%v", n, err)
	}
}

func TestTransparentBothWays(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()

	// Размеры вокруг границ буфера: 1 Б, обычный, больше keepBuf, несколько МиБ.
	var up, down []byte
	for i, n := range []int{1, 100, 4092, 4093, 70000, 3 << 20} {
		up = append(up, frame(n, int64(i))...)
		down = append(down, frame(n+7, int64(100+i))...)
	}
	go func() { cli.Write(up) }()
	go func() { svc.Write(down) }()
	if got := readN(t, svc, len(up)); !bytes.Equal(got, up) {
		t.Error("поток клиент→сервис изменён")
	}
	if got := readN(t, cli, len(down)); !bytes.Equal(got, down) {
		t.Error("поток сервис→клиент изменён")
	}
}

// Один байт за раз: кадр, собранный из кусочков, пересылается так же, как целый.
func TestByteAtATime(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()
	f := append(frame(300, 1), frame(2, 2)...)
	go func() {
		for i := range f {
			cli.Write(f[i : i+1])
		}
	}()
	if got := readN(t, svc, len(f)); !bytes.Equal(got, f) {
		t.Error("поток изменён")
	}
}

func TestEachClientGetsOwnUpstream(t *testing.T) {
	r := newRig(t, nil)
	c1, s1 := r.connect()
	c2, s2 := r.connect()
	defer c1.Close()
	defer c2.Close()
	defer s1.Close()
	defer s2.Close()
	a, b := frame(10, 1), frame(20, 2)
	s1.Write(a)
	s2.Write(b)
	if !bytes.Equal(readN(t, c1, len(a)), a) || !bytes.Equal(readN(t, c2, len(b)), b) {
		t.Error("кадры сервиса попали не тому клиенту")
	}
}

func TestRawFallback(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()
	// Длина 0xFFFFFFFF — не кадр шины; всё последующее должно дойти без искажений.
	junk := append([]byte{0xFF, 0xFF, 0xFF, 0xFF}, bytes.Repeat([]byte("не кадр шины "), 5000)...)
	go func() { cli.Write(junk) }()
	if got := readN(t, svc, len(junk)); !bytes.Equal(got, junk) {
		t.Error("байты вне формата изменены")
	}
	// Обратное направление не зависит от того, что клиент прислал мусор.
	f := frame(50, 3)
	svc.Write(f)
	if !bytes.Equal(readN(t, cli, len(f)), f) {
		t.Error("кадр сервиса после мусора клиента искажён")
	}
	// Нулевая длина — тоже «не кадр».
	zero := []byte{0, 0, 0, 0, 1, 2, 3}
	svc.Write(zero)
	if !bytes.Equal(readN(t, cli, len(zero)), zero) {
		t.Error("нулевая длина искажена")
	}
}

func TestPartialFrameOnEOF(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()
	part := append([]byte{100, 0, 0, 0}, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10) // обещано 100 Б, пришло 10
	cli.Write(part)
	cli.CloseWrite()
	if !bytes.Equal(readN(t, svc, len(part)), part) {
		t.Error("обрезанный кадр изменён")
	}
	expectEOF(t, svc)
}

func TestHalfCloseAndFullClose(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()
	cli.CloseWrite() // клиент больше не пишет, но ответ получить должен
	expectEOF(t, svc)
	f := frame(30, 4)
	svc.Write(f)
	if !bytes.Equal(readN(t, cli, len(f)), f) {
		t.Error("ответ после закрытия записи клиентом потерян")
	}
	svc.Close()
	expectEOF(t, cli)
}

func TestServiceCloseReachesClient(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	svc.Close()
	expectEOF(t, cli)
}

func TestClientCloseReachesService(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer svc.Close()
	cli.Close()
	expectEOF(t, svc)
}

// expectEOFOrReset: при недоступном сервисе шим рвёт соединение RST, клиент видит сброс либо конец потока.
func expectEOFOrReset(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	if n, err := c.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("ждали обрыв, получили %d Б, err=%v", n, err)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("обрыва не дождались: %v", err)
	}
}

func TestServiceDown(t *testing.T) {
	r := newRig(t, nil)
	r.upstream.Close()
	c, err := net.Dial("tcp", r.proxy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expectEOF(t, c)
}

func TestRefusedIsLoggedRarely(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	r := newRig(t, func(p *Proxy) {
		p.Logf = func(format string, a ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, a...))
			mu.Unlock()
		}
	})
	r.upstream.Close()
	for i := 0; i < 50; i++ {
		c, err := net.Dial("tcp", r.proxy.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		expectEOFOrReset(t, c)
		c.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) > 3 {
		t.Errorf("50 отказов дали %d строк журнала, ждали не больше 3: %q", len(lines), lines)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "сервис недоступен") {
		t.Errorf("отказ не записан в журнал: %q", lines)
	}
}

func TestSourceAddress(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("127.0.0.2 недоступен на этой системе")
	}
	probe.Close()
	r := newRig(t, func(p *Proxy) { p.Src = net.ParseIP("127.0.0.2") })
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()
	if ip := svc.RemoteAddr().(*net.TCPAddr).IP.String(); ip != "127.0.0.2" {
		t.Errorf("сервис видит клиента с адреса %s, ждали 127.0.0.2", ip)
	}
}

func TestTapAndStats(t *testing.T) {
	var st Stats
	r := newRig(t, func(p *Proxy) { p.Tap = st.Tap })
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()

	voice := pb.AppendBytes(nil, 29, []byte("ogg"))
	a := wire.AppendFrame(nil, wire.Wrap(wire.TypeAudioCommand, voice))
	e := wire.AppendFrame(nil, wire.Wrap(wire.TypeAudioEvent, pb.AppendUint(nil, 1, 3)))
	cli.Write(a)
	svc.Write(e)
	readN(t, svc, len(a))
	readN(t, cli, len(e))

	// Tap вызывается после записи получателю, поэтому дождёмся счётчиков.
	deadline := time.Now().Add(wait)
	var out strings.Builder
	for time.Now().Before(deadline) {
		out.Reset()
		st.Report(&out, "окно", false)
		if strings.Contains(out.String(), "AudioCommand/voice-chunk") && strings.Contains(out.String(), "AudioEvent") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := out.String()
	for _, want := range []string{"→сервис AudioCommand/voice-chunk", "→клиент AudioEvent"} {
		if !strings.Contains(s, want) {
			t.Errorf("в сводке нет %q:\n%s", want, s)
		}
	}
	st.Report(io.Discard, "", true)
	out.Reset()
	st.Report(&out, "окно", false)
	if out.Len() != 0 {
		t.Errorf("после сброса сводка не пуста: %q", out.String())
	}
}

func TestCancelClosesConnections(t *testing.T) {
	r := newRig(t, nil)
	cli, svc := r.connect()
	defer cli.Close()
	defer svc.Close()
	r.cancel()
	expectEOF(t, cli)
	expectEOF(t, svc)
	select {
	case <-r.stopped:
		if r.err != nil {
			t.Errorf("Serve вернул %v", r.err)
		}
	case <-time.After(wait):
		t.Fatal("Serve не вернулся после отмены")
	}
}

// Run: пока сервис недоступен, слушатель закрыт и клиентам отказывает ядро; когда сервис вернулся — снова принимает.
func TestRunRefusesWhileServiceDown(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upAddr := up.Addr().String()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()

	p := &Proxy{Listen: addr, Upstream: upAddr, Grace: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			t.Error("Run не вернулся после отмены")
		}
	}()

	dialUntil := func(want bool) net.Conn {
		t.Helper()
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			c, err := net.Dial("tcp", addr)
			if (err == nil) == want {
				return c
			}
			if err == nil {
				c.Close() // попытка, которая обнаружила отказ сервиса и закрыла слушатель
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("за %s не наступило состояние «подключение удаётся = %v»", wait, want)
		return nil
	}
	accept := func(l net.Listener) net.Conn {
		t.Helper()
		l.(*net.TCPListener).SetDeadline(time.Now().Add(wait))
		s, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Перед тем как слушать, шим проверяет сервис пробным соединением (его сразу закрывает): забираем его.
	accept(up).Close()
	// сервис жив: данные ходят
	c := dialUntil(true)
	s := accept(up)
	f := frame(20, 1)
	c.Write(f)
	if !bytes.Equal(readN(t, s, len(f)), f) {
		t.Fatal("кадр искажён при живом сервисе")
	}
	// сервис пропал: клиентам должно отказывать ядро
	up.Close()
	s.Close()
	expectEOF(t, c)
	c.Close()
	dialUntil(false)
	// сервис вернулся: шим снова принимает
	up2, err := net.Listen("tcp", upAddr)
	if err != nil {
		t.Skipf("порт сервиса не удалось занять заново: %v", err)
	}
	defer up2.Close()
	accept(up2).Close() // пробное соединение шима
	c = dialUntil(true)
	defer c.Close()
	s = accept(up2)
	defer s.Close()
	c.Write(f)
	if !bytes.Equal(readN(t, s, len(f)), f) {
		t.Fatal("кадр искажён после возвращения сервиса")
	}
}
