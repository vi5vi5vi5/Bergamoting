package music

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func encrypt(key, plain []byte) []byte {
	blk, _ := aes.NewCipher(key)
	out := make([]byte, len(plain))
	cipher.NewCTR(blk, make([]byte, 16)).XORKeyStream(out, plain)
	return out
}

func testBlob(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestOpenDecryptsFromAnyOffset(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := testBlob(5000)
	enc := encrypt(key, plain)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "t.mp4", time.Time{}, bytes.NewReader(enc))
	}))
	defer srv.Close()
	for _, off := range []int64{0, 1, 15, 16, 17, 31, 1000, 4999} {
		var got atomic.Uint64
		s, err := Open(context.Background(), srv.Client(), []string{srv.URL}, key, off, &got)
		if err != nil {
			t.Fatalf("смещение %d: %v", off, err)
		}
		data, err := io.ReadAll(s)
		s.Close()
		if err != nil {
			t.Fatalf("смещение %d: %v", off, err)
		}
		if !bytes.Equal(data, plain[off:]) {
			t.Fatalf("смещение %d: расшифровано неверно (счётчик AES не продолжен с нужного блока)", off)
		}
		if got.Load() != uint64(len(data)) {
			t.Fatalf("смещение %d: счётчик байт %d, прочитано %d", off, got.Load(), len(data))
		}
		if s.Total != int64(len(plain)) {
			t.Fatalf("смещение %d: Total=%d, ждали %d", off, s.Total, len(plain))
		}
	}
}

func TestOpenWrongKeyGarbles(t *testing.T) {
	enc := encrypt([]byte("0123456789abcdef"), testBlob(100))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(enc) }))
	defer srv.Close()
	s, err := Open(context.Background(), srv.Client(), []string{srv.URL}, []byte("fedcba9876543210"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(s)
	s.Close()
	if bytes.Equal(data, testBlob(100)) {
		t.Fatal("с чужим ключом открытый текст получиться не должен")
	}
}

func TestOpenFailsOverToMirror(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := testBlob(300)
	enc := encrypt(key, plain)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "нет", 404) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(enc) }))
	defer good.Close()
	s, err := Open(context.Background(), good.Client(), []string{bad.URL, "", good.URL}, key, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.URL != good.URL {
		t.Fatalf("открыт %s, а должно быть зеркало %s", s.URL, good.URL)
	}
	data, _ := io.ReadAll(s)
	if !bytes.Equal(data, plain) {
		t.Fatal("данные с зеркала расшифрованы неверно")
	}
	if _, err := Open(context.Background(), bad.Client(), []string{bad.URL}, key, 0, nil); err == nil {
		t.Fatal("единственный URL с 404 должен давать ошибку")
	}
	if _, err := Open(context.Background(), bad.Client(), []string{bad.URL}, key[:5], 0, nil); err == nil {
		t.Fatal("короткий ключ должен давать ошибку")
	}
}

func TestOpenCancelStopsReading(t *testing.T) {
	key := []byte("0123456789abcdef")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 100))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Open(ctx, srv.Client(), []string{srv.URL}, key, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	buf := make([]byte, 100)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Read(buf); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("после отмены чтение должно вернуть ошибку")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("отмена не прервала зависшее чтение")
	}
}
