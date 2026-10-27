package shim

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openmediad/internal/wire"
)

// proxyOf достаёт Proxy, созданный стендом: tune вызывается до запуска, там его и запоминаем.
func rigWith(t *testing.T, tune func(*Proxy)) (*rig, *Proxy) {
	var p *Proxy
	r := newRig(t, func(x *Proxy) {
		p = x
		if tune != nil {
			tune(x)
		}
	})
	return r, p
}

func TestInterceptEatsSomeFramesAndPassesOthers(t *testing.T) {
	eaten := frame(40, 1)
	passed := frame(60, 2)
	var seen [][]byte
	var mu sync.Mutex
	r, _ := rigWith(t, func(p *Proxy) {
		p.Intercept = func(c *Conn, f []byte) bool {
			mu.Lock()
			seen = append(seen, append([]byte(nil), f...))
			mu.Unlock()
			return bytes.Equal(f, eaten[4:])
		}
	})
	cli, svc := r.connect()
	cli.Write(eaten)
	cli.Write(passed)
	got := readN(t, svc, len(passed))
	if !bytes.Equal(got, passed) {
		t.Fatal("сервис должен получить только не перехваченный кадр, без искажений")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !bytes.Equal(seen[0], eaten[4:]) || !bytes.Equal(seen[1], passed[4:]) {
		t.Fatalf("перехватчик должен видеть оба кадра по порядку, увидел %d", len(seen))
	}
}

func TestInterceptNotCalledForServiceFrames(t *testing.T) {
	var calls atomic.Int32
	r, _ := rigWith(t, func(p *Proxy) {
		p.Intercept = func(*Conn, []byte) bool { calls.Add(1); return true }
	})
	cli, svc := r.connect()
	f := frame(50, 3)
	svc.Write(f)
	if got := readN(t, cli, len(f)); !bytes.Equal(got, f) {
		t.Fatal("кадры сервиса клиенту должны идти без перехвата")
	}
	if calls.Load() != 0 {
		t.Fatal("перехватчик не должен вызываться для кадров сервиса")
	}
}

func TestBroadcastReachesAllClients(t *testing.T) {
	r, p := rigWith(t, nil)
	c1, _ := r.connect()
	c2, _ := r.connect()
	c3, _ := r.connect()
	time.Sleep(50 * time.Millisecond)
	msg := wire.Wrap(wire.TypeAudioEvent, []byte("событие"))
	if n := p.Broadcast(msg); n != 3 {
		t.Fatalf("рассылка дошла до %d клиентов из 3", n)
	}
	want := wire.AppendFrame(nil, msg)
	for i, c := range []*net.TCPConn{c1, c2, c3} {
		if got := readN(t, c, len(want)); !bytes.Equal(got, want) {
			t.Fatalf("клиент %d получил искажённый кадр", i)
		}
	}
}

// TestBroadcastDoesNotInterleaveWithServiceFrames: рассылка шима и кадры сервиса идут в один сокет клиента;
// кадры не должны перемешиваться посреди кадра.
func TestBroadcastDoesNotInterleaveWithServiceFrames(t *testing.T) {
	r, p := rigWith(t, nil)
	cli, svc := r.connect()
	time.Sleep(50 * time.Millisecond)
	const n = 300
	big := make([]byte, 20000)
	for i := range big {
		big[i] = byte(i)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			svc.Write(wire.AppendFrame(nil, append([]byte{'S'}, big...)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			p.Broadcast(append([]byte{'B'}, big...))
		}
	}()
	counts := map[byte]int{}
	fr := wire.NewFrameReader(cli)
	cli.SetReadDeadline(time.Now().Add(20 * time.Second))
	for i := 0; i < 2*n; i++ {
		f, err := fr.Next()
		if err != nil {
			t.Fatalf("кадр %d: %v", i, err)
		}
		if len(f) != 1+len(big) || !bytes.Equal(f[1:], big) {
			t.Fatalf("кадр %d испорчен (длина %d): записи перемешались", i, len(f))
		}
		counts[f[0]]++
	}
	wg.Wait()
	if counts['S'] != n || counts['B'] != n {
		t.Fatalf("получено кадров сервиса %d и рассылки %d, ждали по %d", counts['S'], counts['B'], n)
	}
}

func TestForwardToWritesToUpstreamOfThatConn(t *testing.T) {
	var got *Conn
	var mu sync.Mutex
	r, p := rigWith(t, func(p *Proxy) {
		p.Intercept = func(c *Conn, f []byte) bool {
			mu.Lock()
			got = c
			mu.Unlock()
			return true
		}
	})
	cliA, svcA := r.connect()
	_, svcB := r.connect()
	cliA.Write(frame(30, 7))
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	c := got
	mu.Unlock()
	if c == nil {
		t.Fatal("перехватчик не вызван")
	}
	msg := []byte("вернуть mediad")
	if err := p.ForwardTo(c, msg); err != nil {
		t.Fatal(err)
	}
	want := wire.AppendFrame(nil, msg)
	if b := readN(t, svcA, len(want)); !bytes.Equal(b, want) {
		t.Fatal("кадр должен прийти на сервисную сторону того же соединения")
	}
	svcB.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if n, _ := svcB.Read(make([]byte, 1)); n != 0 {
		t.Fatal("чужое соединение ничего получать не должно")
	}
}

func TestBroadcastSkipsClosedConnections(t *testing.T) {
	r, p := rigWith(t, nil)
	c1, _ := r.connect()
	c2, s2 := r.connect()
	_ = c2
	time.Sleep(50 * time.Millisecond)
	c1.Close()
	s2.Close()
	time.Sleep(300 * time.Millisecond)
	if n := p.Broadcast([]byte("x")); n > 1 {
		t.Fatalf("рассылка дошла до %d клиентов, закрытые не должны получать", n)
	}
}
