package music

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// vendorCA — набор корневых сертификатов прошивки: у Go на колонке нет системного хранилища в привычном месте.
const vendorCA = "/system/vendor/quasar/ca-certificates.crt"

// NewClient — HTTP-клиент для потоков Яндекса: сначала системные корни, при их отсутствии — набор прошивки.
func NewClient() *http.Client {
	pool, _ := x509.SystemCertPool()
	if pool == nil || len(pool.Subjects()) == 0 { //nolint:staticcheck // Subjects устарел, но нужен ровно для проверки пустоты
		pool = x509.NewCertPool()
		if pem, err := os.ReadFile(vendorCA); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext:           (&net.Dialer{Timeout: 6 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   6 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
	}}
}

// Stream — расшифрованный поток трека, начатый с заданного смещения.
type Stream struct {
	io.Reader
	Total int64 // полный размер потока, 0 — неизвестен
	URL   string
	got   *atomic.Uint64
	close func()
}

func (s *Stream) Close() error { s.close(); return nil }

// Read считает полученные байты (они идут в diag-событие conbytes).
func (s *Stream) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.got.Add(uint64(n))
	return n, err
}

// idleReader рвёт запрос, если поток молчит дольше idle.
type idleReader struct {
	r      io.Reader
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelFunc
}

func (i *idleReader) Read(p []byte) (int, error) {
	i.timer.Reset(i.idle)
	n, err := i.r.Read(p)
	i.timer.Reset(i.idle)
	return n, err
}

// Open открывает поток с offset байт, перебирая зеркала по порядку, и расшифровывает его (AES-128-CTR с нулевым
// начальным счётчиком: счётчик равен номеру 16-байтного блока от начала файла, поэтому с любого места).
func Open(ctx context.Context, cl *http.Client, urls []string, key []byte, offset int64, got *atomic.Uint64) (*Stream, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("ключ потока %d байт, нужно 16", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, u := range urls {
		if u == "" {
			continue
		}
		s, err := openOne(ctx, cl, blk, u, offset, got)
		if err == nil {
			return s, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("нет ни одного URL")
	}
	return nil, lastErr
}

func openOne(ctx context.Context, cl *http.Client, blk cipher.Block, url string, offset int64, got *atomic.Uint64) (*Stream, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, "GET", url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := cl.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	want := http.StatusOK
	if offset > 0 {
		want = http.StatusPartialContent
	}
	if resp.StatusCode != want {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%s: HTTP %d (ждали %d)", hostOf(url), resp.StatusCode, want)
	}
	total := resp.ContentLength
	if offset > 0 {
		total = 0
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndexByte(cr, '/'); i >= 0 {
				total, _ = strconv.ParseInt(cr[i+1:], 10, 64)
			}
		}
	}
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/16))
	ctr := cipher.NewCTR(blk, iv)
	if skip := offset % 16; skip > 0 {
		ctr.XORKeyStream(make([]byte, skip), make([]byte, skip))
	}
	idle := &idleReader{r: resp.Body, idle: 20 * time.Second, cancel: cancel}
	idle.timer = time.AfterFunc(idle.idle, cancel)
	if got == nil {
		got = new(atomic.Uint64)
	}
	return &Stream{
		Reader: cipher.StreamReader{S: ctr, R: idle},
		Total:  total,
		URL:    url,
		got:    got,
		close: func() {
			idle.timer.Stop()
			cancel()
			resp.Body.Close()
		},
	}, nil
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}
