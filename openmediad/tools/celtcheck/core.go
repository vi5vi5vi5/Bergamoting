package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	opus "github.com/tphakala/go-opus/opus"
)

// runCore измеряет сам декодер без обвязки стенда: переиспользуемый буфер, без накопления вывода,
// без эталонов. Три фазы: один долгоживущий декодер; новый декодер на каждый ответ; состояние после GC.
func runCore(rounds int, files []string) {
	fmt.Println("старт процесса:                 ", memLine())
	var streams [][][]byte
	var audioSec float64
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		_, pkts, err := parseOgg(b)
		if err != nil {
			fmt.Println(f, err)
			os.Exit(1)
		}
		streams = append(streams, pkts)
		audioSec += float64(len(pkts)) * 0.02
	}
	buf := make([]int16, 5760)

	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	d, err := opus.NewDecoder(48000, 1)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	runtime.ReadMemStats(&m1)
	fmt.Printf("один Decoder: ≈%d КБ кучи (HeapAlloc %d → %d КБ)\n", (m1.HeapAlloc-m0.HeapAlloc)/1024, m0.HeapAlloc/1024, m1.HeapAlloc/1024)
	fmt.Println("после NewDecoder:               ", memLine())

	t0 := time.Now()
	var allocs0 runtime.MemStats
	runtime.ReadMemStats(&allocs0)
	for r := 0; r < rounds; r++ {
		for _, pkts := range streams {
			d.Reset()
			for _, p := range pkts {
				if _, err := d.Decode(p, buf); err != nil {
					fmt.Println("decode:", err)
					os.Exit(1)
				}
			}
		}
	}
	el := time.Since(t0)
	var allocs1 runtime.MemStats
	runtime.ReadMemStats(&allocs1)
	total := audioSec * float64(rounds)
	fmt.Printf("A) долгоживущий декодер, %d раундов (%.1f с звука): %.1f мс на 1 с звука (%.0f× реального времени), аллокаций на 1 с звука: %.1f (%.1f КБ)\n",
		rounds, total, float64(el.Milliseconds())/total, total/el.Seconds(),
		float64(allocs1.Mallocs-allocs0.Mallocs)/total, float64(allocs1.TotalAlloc-allocs0.TotalAlloc)/1024/total)
	fmt.Println("   память после A:              ", memLine())

	t1 := time.Now()
	for r := 0; r < rounds; r++ {
		for _, pkts := range streams {
			nd, _ := opus.NewDecoder(48000, 1)
			for _, p := range pkts {
				if _, err := nd.Decode(p, buf); err != nil {
					fmt.Println("decode:", err)
					os.Exit(1)
				}
			}
		}
	}
	el2 := time.Since(t1)
	fmt.Printf("B) новый декодер на каждый ответ, %d раундов: %.1f мс на 1 с звука\n", rounds, float64(el2.Milliseconds())/total)
	fmt.Println("   память после B:              ", memLine())

	d = nil
	runtime.GC()
	debug.FreeOSMemory()
	fmt.Println("C) после GC + FreeOSMemory:     ", memLine())
}
