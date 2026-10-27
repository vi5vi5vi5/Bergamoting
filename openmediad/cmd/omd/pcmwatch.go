package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"openmediad/internal/clock"
	"openmediad/internal/pcm"
)

// pcmWatch печатает каждую смену состояния PCM воспроизведения и trigger_time (момент запуска/остановки
// потока ядром, CLOCK_MONOTONIC) вместе с нашим монотонным временем. Вместе с `observe -mono` даёт одну
// шкалу времени для штатного пути Алисы: событие плеера → запуск PCM.
func pcmWatch(args []string) error {
	fs := flag.NewFlagSet("pcmwatch", flag.ExitOnError)
	dur := fs.Duration("for", 60*time.Second, "сколько наблюдать")
	fs.Parse(args)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	end := time.Now().Add(*dur)

	var buf [512]byte
	prev, prevTrig := "", int64(-1)
	for time.Now().Before(end) {
		select {
		case <-sig:
			return nil
		default:
		}
		state, trig, err := pcm.Read(buf[:])
		if err != nil {
			return err
		}
		now := clock.Mono()
		if state != prev || trig != prevTrig {
			from := prev
			if from == "" {
				from = "?"
			}
			fmt.Printf("m%.4f PCM %s → %s, trigger_time m%.6f\n", float64(now)/1e9, from, state, float64(trig)/1e9)
			prev, prevTrig = state, trig
		}
		time.Sleep(500 * time.Microsecond)
	}
	return nil
}
