package player

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"time"

	"openmediad/internal/pb"
)

func float64frombits(b uint64) float64 { return math.Float64frombits(b) }

func unhex(s string) []byte {
	if len(s)%2 != 0 {
		return nil
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		var v byte
		for _, c := range []byte(s[2*i : 2*i+2]) {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'a' && c <= 'f':
				v |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				v |= c - 'A' + 10
			default:
				return nil
			}
		}
		b[i] = v
	}
	return b
}

// Diag собирает записи поля 14 (диагностика загрузки), как это делает mediad.
func Diag(mark, vsid string, got, total uint64, ended bool, playedSec uint64) []byte {
	num := func(k string, v uint64) []byte {
		return pb.AppendBytes(nil, 14, pb.AppendUint(pb.AppendString(nil, 1, k), 3, v))
	}
	str := func(k, v string) []byte {
		return pb.AppendBytes(nil, 14, pb.AppendString(pb.AppendString(nil, 1, k), 2, v))
	}
	var b []byte
	b = append(b, str("mark", mark)...)
	b = append(b, str("vsid", vsid)...)
	b = append(b, num("buffering", 0)...)
	b = append(b, num("rss_mem", 33000)...)
	b = append(b, num("rss_lim", 48800)...)
	b = append(b, num("bitrate", 192)...)
	b = append(b, num("conbytes", got)...)
	b = append(b, num("conconlen", total)...)
	b = append(b, num("conpos", got)...)
	b = append(b, num("conretry", 0)...)
	b = append(b, str("constate", "GST_SOUP_HTTP_SRC_CONNECTION_STATE_REQUEST_BODY_DOWNLOADING")...)
	if ended {
		b = append(b, num("played_sec", playedSec)...)
	}
	return b
}

func randIn(alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

func randString(n int) string {
	return randIn("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", n)
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newVsid — идентификатор сессии просмотра, как у mediad: 52 символа [a-z0-9], «STMx0000x» и время в секундах.
func newVsid(now time.Time) string {
	return randIn("0123456789abcdefghijklmnopqrstuvwxyz", 52) + "STMx0000x" + fmt.Sprint(now.Unix())
}
