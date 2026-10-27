package spectrum

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/cmplx"
	"math/rand"
	"os"
	"testing"

	"openmediad/internal/obs"
	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

func TestFFTMatchesNaiveDFT(t *testing.T) {
	a := New(nil)
	r := rand.New(rand.NewSource(1))
	x := make([]complex128, NFFT)
	for i := range x {
		x[i] = complex(r.NormFloat64(), r.NormFloat64())
	}
	want := make([]complex128, NFFT)
	for k := range want {
		for n, v := range x {
			want[k] += v * cmplx.Rect(1, -2*math.Pi*float64(k*n)/NFFT)
		}
	}
	fftInPlace(x, a.tw[:])
	for k := range x {
		if cmplx.Abs(x[k]-want[k]) > 1e-8 {
			t.Fatalf("бин %d: %v, ждали %v", k, x[k], want[k])
		}
	}
}

// pcm строит стерео S16LE из функции отсчёта.
func pcm(n int, f func(i int) float64) []byte {
	b := make([]byte, 4*n)
	for i := 0; i < n; i++ {
		v := uint16(int16(math.Round(f(i))))
		binary.LittleEndian.PutUint16(b[4*i:], v)
		binary.LittleEndian.PutUint16(b[4*i+2:], v)
	}
	return b
}

func collect(a *Analyzer) *[][]float32 {
	var out [][]float32
	a.Emit = func(db []float32) { out = append(out, append([]float32(nil), db...)) }
	return &out
}

func TestToneLandsInRightBand(t *testing.T) {
	a := New(nil)
	frames := collect(a)
	const bin = 100
	a.Write(pcm(FrameLen, func(i int) float64 { return 10000 * math.Sin(2*math.Pi*bin*float64(i)/NFFT) }))
	if len(*frames) != 1 {
		t.Fatalf("за 40 мс ждали один кадр, получено %d", len(*frames))
	}
	db := (*frames)[0]
	peak := 0
	for i, v := range db {
		if v > db[peak] {
			peak = i
		}
	}
	if peak != bin {
		t.Fatalf("пик в полосе %d, ждали %d", peak, bin)
	}
	// амплитуда A=10000/32767: окно Хэмминга даёт |X| = A/2·Σw/N ≈ A·0,27, мощность /N² уже учтена: ≈ 20·lg(0,305·0,54)
	want := 20 * math.Log10(10000.0/32767*0.54/2)
	if math.Abs(float64(db[bin])-want) > 0.5 {
		t.Fatalf("уровень пика %.2f дБ, ждали ≈%.2f", db[bin], want)
	}
	if db[bin+40] > db[bin]-30 {
		t.Fatalf("вдали от тона спектр должен быть заметно ниже: %.1f против %.1f", db[bin+40], db[bin])
	}
}

func TestSilenceIsFloor(t *testing.T) {
	a := New(nil)
	frames := collect(a)
	a.Write(make([]byte, 4*FrameLen*3))
	if len(*frames) != 3 {
		t.Fatalf("кадров %d, ждали 3", len(*frames))
	}
	for _, db := range *frames {
		for i, v := range db {
			if v != Threshold {
				t.Fatalf("тишина: полоса %d = %v, ждали порог %v", i, v, float64(Threshold))
			}
		}
	}
}

func TestFramingAndFirstWindowRule(t *testing.T) {
	a := New(nil)
	frames := collect(a)
	tone := func(i int) float64 { return 8000 * math.Sin(2*math.Pi*50*float64(i)/NFFT) }
	a.Write(pcm(FrameLen-1, tone))
	if len(*frames) != 0 {
		t.Fatal("неполный интервал кадра не даёт")
	}
	a.Write(pcm(1, tone))
	if len(*frames) != 1 {
		t.Fatal("ровно интервал даёт кадр")
	}
	// как у GstSpectrum, в расчёт идут только первые 1024 отсчёта интервала: остальные 896 на кадр не влияют
	a2 := New(nil)
	f2 := collect(a2)
	a2.Write(pcm(FrameLen, func(i int) float64 {
		if i >= NFFT {
			return 20000 // шум после окна БПФ
		}
		return tone(i)
	}))
	for i := range (*frames)[0] {
		if (*frames)[0][i] != (*f2)[0][i] {
			t.Fatalf("полоса %d изменилась из-за отсчётов за окном БПФ", i)
		}
	}
	// Reset сбрасывает недобранный интервал
	a.Write(pcm(1000, tone))
	a.Reset()
	a.Write(pcm(FrameLen-1000, tone))
	if len(*frames) != 1 {
		t.Fatal("после Reset недобранные 1000 отсчётов должны быть забыты")
	}
}

// TestMatchesStockSpectrum сверяет с записью штатного спектра на том же треке. Нужны: OMD_SPECTRUM_CAP (pcap со спектром),
// OMD_SPECTRUM_PCM (PCM S16LE стерео 48 кГц с начала трека, как отдаёт декодер до нормализации), OMD_SPECTRUM_STREAM
// (поле 46.7 потока в записи), OMD_SPECTRUM_MONO (непусто для голоса: PCM моно) и OMD_SPECTRUM_GAIN_DB (нормализация трека; штатный путь считает спектр после неё).
// Результат проверки 27.10.2026: «Hot N Cold» −7,541 дБ при предсказанных −7,52; «Like a Prayer» −1,729 при −1,74; разброс 0,02 дБ.
func TestMatchesStockSpectrum(t *testing.T) {
	capFile, pcmFile := os.Getenv("OMD_SPECTRUM_CAP"), os.Getenv("OMD_SPECTRUM_PCM")
	if capFile == "" || pcmFile == "" {
		t.Skip("OMD_SPECTRUM_CAP/PCM не заданы")
	}
	var stream uint64
	var gainDB float64
	fmt.Sscan(os.Getenv("OMD_SPECTRUM_STREAM"), &stream)
	fmt.Sscan(os.Getenv("OMD_SPECTRUM_GAIN_DB"), &gainDB)

	var stock [][]float32
	f, err := os.Open(capFile)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	obs.WalkPcap(f, func(fr obs.Frame) {
		if fr.Svc != "iohub_services" || !fr.ToSvc || stopped {
			return
		}
		m, err := wire.Unwrap(fr.Data)
		if err != nil || m.Type != wire.TypeIOHub {
			return
		}
		b, ok := pb.Bytes(m.Body, 46)
		if !ok {
			return
		}
		if v7, _ := pb.Uint(b, 7); v7 != stream {
			return
		}
		switch k, _ := pb.Uint(b, 6); k {
		case 2:
			stopped = true
		case 0:
			raw, _ := pb.Bytes(b, 4)
			v := make([]float32, len(raw)/4)
			for i := range v {
				v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
			}
			stock = append(stock, v)
		}
	})
	f.Close()
	data, err := os.ReadFile(pcmFile)
	if err != nil {
		t.Fatal(err)
	}
	a := New(nil)
	a.Scale = math.Pow(10, gainDB/20)
	frames := collect(a)
	if os.Getenv("OMD_SPECTRUM_MONO") != "" {
		a.WriteMono(data) // голос Алисы: PCM моно
	} else {
		a.Write(data)
	}
	var sum, sum2 float64
	n := 0
	for k := 5; k < 300 && k < len(stock) && k < len(*frames); k++ {
		for b := 2; b < 300; b++ {
			if stock[k][b] < -55 || (*frames)[k][b] < -55 {
				continue
			}
			d := float64(stock[k][b] - (*frames)[k][b])
			sum += d
			sum2 += d * d
			n++
		}
	}
	if n < 300 {
		t.Fatalf("слишком мало точек для сверки: %d", n)
	}
	mean := sum / float64(n)
	std := math.Sqrt(sum2/float64(n) - mean*mean)
	t.Logf("точек %d, разность штатный−наш %.3f дБ, разброс %.3f дБ", n, mean, std)
	if math.Abs(mean) > 0.1 || std > 0.1 {
		t.Fatalf("спектр расходится со штатным: разность %.3f дБ, разброс %.3f дБ", mean, std)
	}
}

// BenchmarkSecondOfAudio — стоимость обработки одной секунды звука (25 кадров спектра). Запуск на колонке:
// GOOS=linux GOARCH=arm64 go test -c -o spectrum.test ./internal/spectrum && ./spectrum.test -test.bench . -test.run '^$'
func BenchmarkSecondOfAudio(b *testing.B) {
	data := pcm(Rate, func(i int) float64 { return 9000*math.Sin(float64(i)*0.05) + 3000*math.Sin(float64(i)*0.31) })
	a := New(func([]float32) {})
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Write(data)
	}
}
