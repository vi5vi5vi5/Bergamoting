// Package spectrum — спектр звука для визуализации на дисплее и светодиодах колонки.
//
// Штатный mediad считает его элементом GStreamer `spectrum interval=40000000 bands=513 threshold=-80` на каждом
// воспроизводимом потоке и 25 раз в секунду шлёт сервису iohub сообщение с 513 значениями в дБ. Шим, играющий музыку
// сам, обязан отдавать то же, иначе анимация пропадает. Здесь БПФ по тем же правилам; точные константы (входной
// масштаб, окно) подобраны по записям штатного спектра на тех же треках (см. PLAN.md, раздел 18).
package spectrum

import (
	"encoding/binary"
	"math"
	"math/cmplx"
	"time"
)

const (
	Bands     = 513              // число полос: nfft/2 + 1
	NFFT      = 2 * (Bands - 1)  // 1024 отсчёта на одно БПФ
	Rate      = 48000            // частота дискретизации входа
	FrameLen  = Rate * 40 / 1000 // отсчётов в интервале 40 мс
	Threshold = -80.0            // нижний порог, дБ
	frameSize = 4                // байт на кадр S16LE стерео
)

// Вид события потока спектра (поле 46.6 сообщения iohub).
const (
	KindData   = 0 // кадр спектра
	KindStart  = 1 // поток начался
	KindStop   = 2 // поток остановлен (пауза, конец, вытеснение)
	KindResume = 3 // поток продолжен после паузы
)

// Источник звука (поле 46.8 сообщения iohub).
const (
	SourceMusic = 0
	SourceVoice = 1 // голос Алисы
)

// Event — событие потока спектра: кадр либо смена состояния. Выдают движки музыки и голоса, отправляет iohub-клиент.
type Event struct {
	Kind   int
	Source int       // SourceMusic или SourceVoice
	Stream uint64    // идентификатор потока (CLOCK_MONOTONIC в наносекундах на момент старта)
	DB     []float32 // 513 значений в дБ для KindData; получатель копирует: срез переиспользуется
	At     time.Time // когда кадр посчитан
}

// Analyzer накапливает PCM (S16LE, 48 кГц, стерео) и по заполнении интервала в 40 мс отдаёт спектр. Не потокобезопасен.
//
// Как считает GstSpectrum (подтверждено сверкой со штатным спектром на двух треках: разброс 0,02–0,05 дБ): отсчёты
// усредняются по каналам и делятся на 32767; окно Хэмминга 0,54−0,46·cos(2πi/(N−1)); счётчик кадров обнуляется после каждого
// сообщения, БПФ делается один раз на интервал, по его первым 1024 отсчётам (остальные 896 в расчёт не идут); мощность
// |X|²/NFFT² переводится в дБ (10·lg) и ограничивается порогом −80.
type Analyzer struct {
	// Scale умножает отсчёты, переведённые в диапазон [-1, 1]. Подбирается по штатному спектру.
	Scale float64
	// Emit получает готовый кадр: Bands значений в дБ (не меньше Threshold). Срез действителен до следующего вызова.
	Emit func(db []float32)

	buf     [NFFT]complex128
	n       int // отсчётов в текущем интервале
	powered bool
	pow     [Bands]float64
	out     [Bands]float32
	win     [NFFT]float64
	tw      [NFFT / 2]complex128
}

// New создаёт анализатор с масштабом 1.
func New(emit func(db []float32)) *Analyzer {
	a := &Analyzer{Scale: 1, Emit: emit}
	for i := range a.win {
		a.win[i] = 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(NFFT-1))
	}
	for i := range a.tw {
		a.tw[i] = cmplx.Rect(1, -2*math.Pi*float64(i)/float64(NFFT))
	}
	return a
}

// Reset забывает накопленное (смена трека, перемотка).
func (a *Analyzer) Reset() {
	a.n, a.powered = 0, false
}

// Write принимает PCM S16LE стерео; хвост, не кратный кадру, отбрасывается.
func (a *Analyzer) Write(pcm []byte) {
	for ; len(pcm) >= frameSize; pcm = pcm[frameSize:] {
		l := float64(int16(binary.LittleEndian.Uint16(pcm)))
		r := float64(int16(binary.LittleEndian.Uint16(pcm[2:])))
		a.sample((l + r) / 2)
	}
}

// WriteMono принимает PCM S16LE моно (голос Алисы: у mediad на спектр идёт моно-поток декодера Opus без смешения каналов).
func (a *Analyzer) WriteMono(pcm []byte) {
	for ; len(pcm) >= 2; pcm = pcm[2:] {
		a.sample(float64(int16(binary.LittleEndian.Uint16(pcm))))
	}
}

// sample добавляет один отсчёт (в единицах int16) в интервал и, когда набралось нужное, считает БПФ и кадр.
func (a *Analyzer) sample(v float64) {
	if a.n < NFFT {
		a.buf[a.n] = complex(v/32767*a.Scale*a.win[a.n], 0)
	}
	a.n++
	if a.n == NFFT {
		fftInPlace(a.buf[:], a.tw[:])
		for i := 0; i < Bands; i++ {
			re, im := real(a.buf[i]), imag(a.buf[i])
			a.pow[i] = (re*re + im*im) / (NFFT * NFFT)
		}
		a.powered = true
	}
	if a.n == FrameLen {
		a.frame()
	}
}

func (a *Analyzer) frame() {
	for i := range a.out {
		v := float64(Threshold)
		if a.powered && a.pow[i] > 0 {
			if d := 10 * math.Log10(a.pow[i]); d > Threshold { // ветвление вместо math.Max: вызов функции на каждую из 513 полос заметен на колонке
				v = d
			}
		}
		a.out[i] = float32(v)
	}
	a.n, a.powered = 0, false
	if a.Emit != nil {
		a.Emit(a.out[:])
	}
}

// fftInPlace — БПФ по основанию 2 с прореживанием по времени; len(x) — степень двойки, tw — повороты e^{-2πik/N}, k < N/2.
func fftInPlace(x []complex128, tw []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ { // перестановка с обращением битов
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half, step := size/2, n/size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				t := tw[k*step] * x[start+k+half]
				x[start+k+half] = x[start+k] - t
				x[start+k] += t
			}
		}
	}
}
