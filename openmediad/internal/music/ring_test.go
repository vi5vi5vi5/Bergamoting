package music

import (
	"bytes"
	"testing"
)

func fill(r *ring, b []byte) {
	if _, err := r.Write(b); err != nil {
		panic(err)
	}
}

func drain(t *testing.T, r *ring, n int) []byte {
	t.Helper()
	out := make([]byte, 0, n)
	buf := make([]byte, 7) // нечётная порция: граница обхода буфера не совпадает с границей чтения
	for len(out) < n {
		k, err := r.ReadWait(buf[:min(len(buf), n-len(out))])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, buf[:k]...)
	}
	return out
}

func TestRingRewindReturnsConsumedBytes(t *testing.T) {
	r := newRing(40, 30)
	data := make([]byte, 40)
	for i := range data {
		data[i] = byte(i)
	}
	fill(r, data)
	if got := drain(t, r, 25); !bytes.Equal(got, data[:25]) {
		t.Fatal("чтение исказило данные")
	}
	if k := r.Rewind(10); k != 10 {
		t.Fatalf("Rewind вернул %d, ждали 10", k)
	}
	if got := drain(t, r, 10); !bytes.Equal(got, data[15:25]) {
		t.Fatalf("после Rewind повторно читаются байты 15..24, получено %v", got)
	}
}

// История — это всё прочитанное, что запись ещё не затёрла: пока обход буфера не начался, возвращается всё прочитанное,
// после него — последние len(buf)−n байт.
func TestRingRewindLimitedByWhatWasNotOverwritten(t *testing.T) {
	r := newRing(40, 30) // всего 70 байт
	var all []byte
	next := byte(0)
	put := func(n int) {
		b := make([]byte, n)
		for i := range b {
			b[i] = next
			next++
		}
		all = append(all, b...)
		fill(r, b)
	}
	put(40)
	drain(t, r, 40)
	if k := r.Rewind(1000); k != 40 {
		t.Fatalf("прочитано 40 байт и ничего не затёрто: вернуть можно 40, вернули %d", k)
	}
	drain(t, r, 40)
	put(40)
	drain(t, r, 40)
	put(40)
	drain(t, r, 40) // прочитано 120 байт при буфере в 70: старое затёрто
	k := r.Rewind(1000)
	if k != 70 {
		t.Fatalf("после обхода буфера остаются последние 70 байт, а вернули %d", k)
	}
	if got := drain(t, r, 70); !bytes.Equal(got, all[len(all)-70:]) {
		t.Fatal("история после обхода буфера искажена")
	}
	if r.Rewind(0) != 0 || r.Rewind(-5) != 0 {
		t.Fatal("нулевой и отрицательный возврат ничего не делают")
	}
}

func TestRingWriterNeverExceedsLimit(t *testing.T) {
	r := newRing(40, 30)
	done := make(chan struct{})
	go func() {
		fill(r, make([]byte, 100)) // больше предела: должна встать
		close(done)
	}()
	for i := 0; i < 100 && r.Len() < 40; i++ {
		sleepShort()
	}
	if r.Len() != 40 {
		t.Fatalf("писатель должен остановиться на пределе 40, а в буфере %d", r.Len())
	}
	select {
	case <-done:
		t.Fatal("запись сверх предела не должна завершаться, пока не прочитали")
	default:
	}
	drain(t, r, 60)
	<-done
}
