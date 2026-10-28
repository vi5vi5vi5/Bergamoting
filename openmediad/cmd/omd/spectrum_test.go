package main

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"openmediad/internal/iohub"
	"openmediad/internal/spectrum"
)

type sentMsg struct {
	at  time.Time
	msg []byte
}

type fakeIohub struct {
	mu   sync.Mutex
	sent []sentMsg
}

func (f *fakeIohub) Send(msg []byte) bool {
	f.mu.Lock()
	f.sent = append(f.sent, sentMsg{time.Now(), append([]byte(nil), msg...)})
	f.mu.Unlock()
	return true
}

func (f *fakeIohub) got() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

// Штатный mediad отправляет кадр сразу, а в метке времени стоит момент, когда звук прозвучит (приход − метка ≈ −0,29 с в
// записях). Первый вариант шима держал кадр и ставил время расчёта: метка оказывалась в прошлом, и платформа не рисовала
// анимацию. Проверяется и порядок: старт, кадры, остановка.
func TestSpecSenderFrameGoesNowStampedForPlayback(t *testing.T) {
	const delay = 120 * time.Millisecond
	f := &fakeIohub{}
	s := newSpecSender(f, delay, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	db := make([]float32, 513)
	for i := range db {
		db[i] = -80 + float32(i%40)
	}
	t0 := time.Now()
	s.Add(spectrum.Event{Kind: iohub.KindStart, Source: iohub.SourceMusic, Stream: 7})
	s.Add(spectrum.Event{Kind: iohub.KindData, Source: iohub.SourceMusic, Stream: 7, DB: db, At: t0})
	s.Add(spectrum.Event{Kind: iohub.KindStop, Source: iohub.SourceMusic, Stream: 7})

	deadline := time.Now().Add(2 * time.Second)
	for len(f.got()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := f.got()
	if len(got) != 3 {
		t.Fatalf("ушло %d сообщений из 3", len(got))
	}
	if !bytes.Equal(got[0].msg, iohub.Control(uint64(iohub.KindStart), 7, uint64(iohub.SourceMusic))) {
		t.Fatal("первым должен идти старт потока")
	}
	wantData := iohub.Data(uint64(t0.Add(delay).UnixNano()), db, 7, uint64(iohub.SourceMusic))
	if !bytes.Equal(got[1].msg, wantData) {
		t.Fatal("кадр: метка времени должна быть моментом расчёта + delay (когда звук прозвучит)")
	}
	if d := got[1].at.Sub(t0); d > delay/2 {
		t.Fatalf("кадр ушёл через %v: он не должен ждать (штатный mediad отправляет сразу)", d)
	}
	if !bytes.Equal(got[2].msg, iohub.Control(uint64(iohub.KindStop), 7, uint64(iohub.SourceMusic))) {
		t.Fatal("последней должна идти остановка потока")
	}
	if d := got[2].at.Sub(t0); d < delay-20*time.Millisecond {
		t.Fatalf("остановка ушла через %v: она должна ждать, пока прозвучит последний кадр (%v)", d, delay)
	}
}
