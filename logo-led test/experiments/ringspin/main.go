// ringspin — эксперимент: крутит радугу по 6 зонам подсветки через /dev/fb0
// и меряет, насколько ровно Go держит частоту кадров на колонке.
//
// Сборка:  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" .
// Запуск:  ringspin -fps 60 -dur 5s
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

const zones = 6

func main() {
	fps := flag.Int("fps", 60, "кадров в секунду")
	dur := flag.Duration("dur", 5*time.Second, "длительность")
	bright := flag.Float64("bright", 0.25, "яркость 0..1")
	flag.Parse()

	fb, err := os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer fb.Close()

	// Запоминаем, что было на кольце, чтобы вернуть в конце.
	saved := make([]byte, zones*3)
	if _, err := fb.ReadAt(saved, 0); err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	defer fb.WriteAt(saved, 0)

	frame := make([]byte, zones*3)
	period := time.Second / time.Duration(*fps)
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	start := time.Now()
	last := start
	var intervals []time.Duration
	var writeCost time.Duration

	for now := range ticker.C {
		t := now.Sub(start).Seconds()
		if now.Sub(start) > *dur {
			break
		}
		for z := 0; z < zones; z++ {
			hue := math.Mod(t*0.5+float64(z)/zones, 1)
			r, g, b := hsv(hue, 1, *bright)
			frame[z*3+0] = b // порядок байт в кадре — BGR
			frame[z*3+1] = g
			frame[z*3+2] = r
		}
		w := time.Now()
		if _, err := fb.WriteAt(frame, 0); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			return
		}
		writeCost += time.Since(w)
		intervals = append(intervals, now.Sub(last))
		last = now
	}

	intervals = intervals[1:] // первый интервал включает старт
	sort.Slice(intervals, func(i, j int) bool { return intervals[i] < intervals[j] })
	pct := func(p float64) time.Duration { return intervals[int(p*float64(len(intervals)-1))] }

	fmt.Printf("кадров: %d, цель %v между кадрами\n", len(intervals)+1, period)
	fmt.Printf("интервал: min %v  p50 %v  p99 %v  max %v\n", intervals[0], pct(0.5), pct(0.99), intervals[len(intervals)-1])
	fmt.Printf("запись в fb0: в среднем %v на кадр\n", writeCost/time.Duration(len(intervals)+1))
	fmt.Printf("память: %s\n", procStatus("VmRSS"))
}

func hsv(h, s, v float64) (r, g, b byte) {
	i := math.Floor(h * 6)
	f := h*6 - i
	p, q, t := v*(1-s), v*(1-f*s), v*(1-(1-f)*s)
	var rf, gf, bf float64
	switch int(i) % 6 {
	case 0:
		rf, gf, bf = v, t, p
	case 1:
		rf, gf, bf = q, v, p
	case 2:
		rf, gf, bf = p, v, t
	case 3:
		rf, gf, bf = p, q, v
	case 4:
		rf, gf, bf = t, p, v
	default:
		rf, gf, bf = v, p, q
	}
	return byte(rf * 255), byte(gf * 255), byte(bf * 255)
}

func procStatus(key string) string {
	data, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return "?"
}
