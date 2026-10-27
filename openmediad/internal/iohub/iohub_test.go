package iohub

import (
	"bytes"
	"context"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"openmediad/internal/obs"
	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

// TestRebuildRealFrames: каждый настоящий кадр спектра из записей (данные и служебные) пересобирается из своих полей
// байт в байт. Контроль: испорченный сборщик не должен проходить (см. TestRebuildDetectsMutation).
func TestRebuildRealFrames(t *testing.T) {
	dir := os.Getenv("OMD_CAPTURES")
	if dir == "" {
		dir = filepath.Join("..", "..", "captures")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.pcap"))
	if len(files) == 0 {
		t.Skip("нет записей (OMD_CAPTURES)")
	}
	total, data, ctl := 0, 0, 0
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		obs.WalkPcap(f, func(fr obs.Frame) {
			if fr.Svc != "iohub_services" || !fr.ToSvc {
				return
			}
			m, err := wire.Unwrap(fr.Data)
			if err != nil || m.Type != wire.TypeIOHub {
				return
			}
			in, ok := pb.Bytes(m.Body, 46)
			if !ok {
				return
			}
			kind, _ := pb.Uint(in, 6)
			stream, _ := pb.Uint(in, 7)
			source, _ := pb.Uint(in, 8)
			var got []byte
			if raw, ok := pb.Bytes(in, 4); ok {
				ts, _ := pb.Uint(in, 1)
				db := make([]float32, len(raw)/4)
				for i := range db {
					db[i] = math.Float32frombits(uint32(raw[4*i]) | uint32(raw[4*i+1])<<8 | uint32(raw[4*i+2])<<16 | uint32(raw[4*i+3])<<24)
				}
				got = Data(ts, db, stream, source)
				data++
			} else {
				got = Control(kind, stream, source)
				ctl++
			}
			total++
			if !bytes.Equal(got, fr.Data) {
				t.Fatalf("кадр спектра (вид %d, %d Б) пересобран иначе:\n got %x\nwant %x", kind, len(fr.Data), got[:min(40, len(got))], fr.Data[:min(40, len(fr.Data))])
			}
		})
		f.Close()
	}
	if total == 0 {
		t.Fatal("в записях нет сообщений спектра — проверка вхолостую")
	}
	t.Logf("пересобрано побайтно: %d кадров данных и %d служебных", data, ctl)
}

func TestRebuildDetectsMutation(t *testing.T) {
	db := make([]float32, 513)
	for i := range db {
		db[i] = -80
	}
	a := Data(1793123873962283000, db, 7, SourceMusic)
	db[3] = -79.5
	if bytes.Equal(a, Data(1793123873962283000, db, 7, SourceMusic)) {
		t.Fatal("разные данные должны давать разные кадры")
	}
	if c := Control(KindStart, 5, SourceVoice); len(c) >= 40 {
		t.Fatalf("служебное сообщение должно быть коротким, а оно %d Б", len(c))
	}
}

// fakeIOHub — сервер: принимает одно соединение за другим, шлёт «служебку» и складывает полученные кадры.
type fakeIOHub struct {
	ln    net.Listener
	got   chan []byte
	conns chan net.Conn
}

func newFake(t *testing.T) *fakeIOHub {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIOHub{ln: ln, got: make(chan []byte, 64), conns: make(chan net.Conn, 4)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.conns <- c
			go func() { // как настоящий iohub: сам присылает служебные кадры
				for i := 0; i < 200; i++ {
					c.Write(wire.AppendFrame(nil, wire.Wrap(wire.TypeIOHubMisc, []byte("служебка"))))
				}
			}()
			go func() {
				fr := wire.NewFrameReader(c)
				for {
					b, err := fr.Next()
					if err != nil {
						return
					}
					f.got <- append([]byte(nil), b...)
				}
			}()
		}
	}()
	return f
}

func TestClientSendsFramesAndDrains(t *testing.T) {
	srv := newFake(t)
	c := &Client{Addr: srv.ln.Addr().String(), Idle: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	msgs := [][]byte{Control(KindStart, 1, 0), Data(1, make([]float32, 513), 1, 0), Control(KindStop, 1, 0)}
	for _, m := range msgs {
		if !c.Send(m) {
			t.Fatal("очередь не должна быть полной")
		}
	}
	for i, m := range msgs {
		select {
		case b := <-srv.got:
			if !bytes.Equal(b, m) {
				t.Fatalf("сообщение %d исказилось", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("сообщение %d не дошло", i)
		}
	}
}

func TestClientReconnectsAndClosesWhenIdle(t *testing.T) {
	srv := newFake(t)
	c := &Client{Addr: srv.ln.Addr().String(), Idle: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	c.Send(Control(KindStart, 1, 0))
	conn1 := <-srv.conns
	<-srv.got
	// простой: соединение закрывается
	conn1.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1<<16)
	for {
		if _, err := conn1.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("после простоя клиент должен закрыть соединение")
			}
			break
		}
	}
	// новое сообщение: соединение открывается заново
	c.Send(Control(KindStart, 2, 0))
	select {
	case <-srv.conns:
	case <-time.After(3 * time.Second):
		t.Fatal("клиент не переподключился")
	}
	select {
	case <-srv.got:
	case <-time.After(3 * time.Second):
		t.Fatal("сообщение после переподключения не дошло")
	}
}

func TestSendNeverBlocksWithoutServer(t *testing.T) {
	c := &Client{Addr: "127.0.0.1:1", Idle: time.Minute} // порт закрыт
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			c.Send(Control(KindStart, uint64(i), 0))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send завис без сервера")
	}
}
