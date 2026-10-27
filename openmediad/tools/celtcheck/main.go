// celtcheck — сравнение чисто-Go декодеров Opus с эталоном (родной декодер колонки) на реальных чанках голоса Алисы.
//
//	celtcheck [-bench N] [-ref-dir DIR] file.ogg...    (рядом с file.ogg лежит file.ref.pcm: S16LE, моно, 48 кГц)
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tphakala/go-opus/oggopus"
	opus "github.com/tphakala/go-opus/opus"
)

// parseOgg возвращает pre-skip из OpusHead и аудиопакеты (без OpusHead/OpusTags).
func parseOgg(b []byte) (preSkip int, pkts [][]byte, err error) {
	var all [][]byte
	var cur []byte
	for off := 0; off < len(b); {
		if off+27 > len(b) || string(b[off:off+4]) != "OggS" {
			return 0, nil, fmt.Errorf("нет OggS на смещении %d", off)
		}
		nseg := int(b[off+26])
		if off+27+nseg > len(b) {
			return 0, nil, fmt.Errorf("обрезанная таблица сегментов")
		}
		segs := b[off+27 : off+27+nseg]
		p := off + 27 + nseg
		for _, s := range segs {
			if p+int(s) > len(b) {
				return 0, nil, fmt.Errorf("обрезанная страница")
			}
			cur = append(cur, b[p:p+int(s)]...)
			p += int(s)
			if s < 255 {
				all = append(all, cur)
				cur = nil
			}
		}
		off = p
	}
	if len(all) < 3 || !bytes.HasPrefix(all[0], []byte("OpusHead")) {
		return 0, nil, fmt.Errorf("не Ogg Opus")
	}
	return int(binary.LittleEndian.Uint16(all[0][10:12])), all[2:], nil
}

func decodeGoOpus(pkts [][]byte) ([]int16, error) {
	d, err := opus.NewDecoder(48000, 1)
	if err != nil {
		return nil, err
	}
	buf := make([]int16, 5760)
	var out []int16
	for i, p := range pkts {
		n, err := d.Decode(p, buf)
		if err != nil {
			return nil, fmt.Errorf("пакет %d: %w", i, err)
		}
		out = append(out, buf[:n]...)
	}
	return out, nil
}

func decodeOggopus(file []byte) ([]int16, oggopus.Info, error) {
	raw, info, err := oggopus.DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		return nil, info, err
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out, info, nil
}

func readPCM(path string) []int16 {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("нет эталона:", err)
		os.Exit(1)
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// compare ищет смещение, при котором got лучше всего ложится на ref, и считает ошибку.
func compare(ref, got []int16) string {
	const probe = 20000
	best, bestCost := 0, math.MaxFloat64
	for d := -2000; d <= 2000; d++ {
		var cost float64
		n := 0
		for i := 0; i < probe && i < len(ref); i++ {
			j := i + d
			if j < 0 || j >= len(got) {
				continue
			}
			cost += math.Abs(float64(ref[i]) - float64(got[j]))
			n++
		}
		if n > probe/2 && cost/float64(n) < bestCost {
			best, bestCost = d, cost/float64(n)
		}
	}
	// смещение d: ref[i] ↔ got[i+d]
	var maxd, sumsq, sumref float64
	var same, n int
	for i := range ref {
		j := i + best
		if j < 0 || j >= len(got) {
			continue
		}
		df := float64(ref[i]) - float64(got[j])
		if a := math.Abs(df); a > maxd {
			maxd = a
		}
		sumsq += df * df
		sumref += float64(ref[i]) * float64(ref[i])
		if ref[i] == got[j] {
			same++
		}
		n++
	}
	snr := math.Inf(1)
	if sumsq > 0 {
		snr = 10 * math.Log10(sumref/sumsq)
	}
	return fmt.Sprintf("смещение %+d, сравнено %d из %d (эталон) / %d (кандидат) отсчётов; макс |Δ|=%.0f, RMS Δ=%.2f, SNR=%.1f дБ, побитово равных %.2f%%",
		best, n, len(ref), len(got), maxd, math.Sqrt(sumsq/float64(max(n, 1))), snr, 100*float64(same)/float64(max(n, 1)))
}

type result struct {
	name string
	dec  func() ([]int16, error)
}

func bench(name string, iters int, f func() ([]int16, error)) {
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t0 := time.Now()
	var n int
	for i := 0; i < iters; i++ {
		out, err := f()
		if err != nil {
			fmt.Println("  bench:", err)
			return
		}
		n = len(out)
	}
	el := time.Since(t0)
	runtime.ReadMemStats(&m1)
	per := el / time.Duration(iters)
	audio := time.Duration(n) * time.Second / 48000
	fmt.Printf("  %-10s %8s на декод (%.0f× быстрее реального времени), аллокаций %d / %d КБ на декод, куча в работе %d КБ\n",
		name, per.Round(time.Microsecond), float64(audio)/float64(per), (m1.Mallocs-m0.Mallocs)/uint64(iters),
		(m1.TotalAlloc-m0.TotalAlloc)/uint64(iters)/1024, m1.HeapInuse/1024)
}

func main() {
	benchN := flag.Int("bench", 0, "число повторов для замера скорости/памяти (0 — только сверка)")
	core := flag.Int("core", 0, "режим «только ядро»: N раундов декодирования без обвязки стенда, с замером памяти")
	flag.Parse()
	debug.SetGCPercent(25) // как у движка omd
	defer printSelfMem()
	if *core > 0 {
		runCore(*core, flag.Args())
		return
	}
	fmt.Printf("GOOS/GOARCH=%s/%s, %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	for _, path := range flag.Args() {
		file, err := os.ReadFile(path)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		preSkip, pkts, err := parseOgg(file)
		if err != nil {
			fmt.Println(path, err)
			os.Exit(1)
		}
		ref := readPCM(strings.TrimSuffix(path, filepath.Ext(path)) + ".ref.pcm")
		fmt.Printf("\n=== %s: пакетов %d, pre-skip %d, эталон %d отсчётов\n", filepath.Base(path), len(pkts), preSkip, len(ref))

		cands := []result{
			{"go-opus", func() ([]int16, error) { o, err := decodeGoOpus(pkts); return trim(o, preSkip), err }},
			{"gopus", func() ([]int16, error) { o, err := decodeGopus(pkts); return trim(o, preSkip), err }},
			{"oggopus", func() ([]int16, error) { o, _, err := decodeOggopus(file); return o, err }},
		}
		for _, c := range cands {
			out, err := c.dec()
			if err != nil {
				fmt.Printf("  %-10s ОШИБКА: %v\n", c.name, err)
				continue
			}
			fmt.Printf("  %-10s %s\n", c.name, compare(ref, out))
		}
		if *benchN > 0 {
			fmt.Println("  --- скорость и память:")
			for _, c := range cands {
				bench(c.name, *benchN, c.dec)
			}
		}
	}
}

func trim(s []int16, n int) []int16 {
	if n > len(s) {
		return nil
	}
	return s[n:]
}
