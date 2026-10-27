//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"openmediad/internal/clock"
	"openmediad/internal/opusstream"
	"openmediad/internal/pcm"
	"openmediad/internal/sink"
)

// voicePlay проигрывает Ogg/Opus-файл (стандин чанка голоса Алисы) через связку «Go-декодер → pipe → aplay»
// и для каждого прогона печатает моменты событий относительно t0 — «чанк получен».
//
// Главная цифра — trigger: когда ядро реально запустило PCM (trigger_time из /proc/asound, CLOCK_MONOTONIC).
// Она наблюдаема только в тишине: если PCM уже RUNNING из-за другого потока, нового запуска не будет.
// Что слышно в динамике после trigger, этой цифрой не измеряется (см. PLAN, §14).
func voicePlay(args []string) error {
	fs := flag.NewFlagSet("voice-play", flag.ExitOnError)
	var cfg sink.Config
	fs.StringVar(&cfg.Path, "aplay", "/usr/bin/aplay", "путь к aplay")
	fs.StringVar(&cfg.Device, "dev", "default", "ALSA-устройство")
	fs.IntVar(&cfg.Channels, "ch", 1, "каналов у aplay (моно: стерео делает plug ALSA)")
	fs.IntVar(&cfg.BufferUS, "buf", 0, "буфер aplay, мкс (0 — по умолчанию aplay)")
	fs.IntVar(&cfg.PeriodUS, "period", 0, "период aplay, мкс (0 — по умолчанию aplay)")
	fs.IntVar(&cfg.StartDelayUS, "start-delay", 0, "aplay -R, мкс; 1 — стартовать сразу (0 — когда заполнится буфер)")
	prespawn := fs.Bool("prespawn", false, "запустить aplay заранее (как если бы его подняли по voice-start)")
	repeat := fs.Int("repeat", 1, "сколько раз проиграть")
	gap := fs.Duration("gap", 3*time.Second, "пауза между прогонами (дать PCM вернуться в PREPARED)")
	chunk := fs.Int("chunk", 0, "размер куска, которым «приходит» чанк, байт (0 — целиком)")
	label := fs.String("label", "", "метка сценария для вывода")
	decodeOnly := fs.Bool("decode-only", false, "только замер декодирования, без звука")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("нужен один аргумент: файл .ogg")
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	st, err := opusstream.New()
	if err != nil {
		return err
	}
	if *decodeOnly {
		return decodeTiming(st, data)
	}

	fmt.Printf("== %s\n   aplay %s | prespawn=%v chunk=%d\n", *label, strings.Join(cfg.Args(), " "), *prespawn, *chunk)
	var triggers, firsts []time.Duration
	for i := 1; i <= *repeat; i++ {
		r, err := playOnce(st, data, cfg, *prespawn, *chunk)
		if err != nil {
			return fmt.Errorf("прогон %d: %w", i, err)
		}
		trig := "нет (PCM уже RUNNING: играет другой поток)"
		if r.triggerOK {
			trig = r.trigger.Round(10 * time.Microsecond).String()
			triggers = append(triggers, r.trigger)
		}
		firsts = append(firsts, r.first)
		fmt.Printf("   #%d exec=%v первый PCM=%v запись в pipe закончена=%v | trigger PCM=%s | aplay завершился=%v\n", i,
			r.spawn.Round(10*time.Microsecond), r.first.Round(10*time.Microsecond),
			r.written.Round(time.Millisecond), trig, r.exit.Round(time.Millisecond))
		if i < *repeat {
			time.Sleep(*gap)
		}
	}
	if len(triggers) > 0 {
		slices.Sort(triggers)
		fmt.Printf("   итог trigger: мин %v, медиана %v, макс %v (из %d)\n", triggers[0].Round(100*time.Microsecond),
			triggers[len(triggers)/2].Round(100*time.Microsecond), triggers[len(triggers)-1].Round(100*time.Microsecond), len(triggers))
	}
	slices.Sort(firsts)
	fmt.Printf("   итог первый PCM: медиана %v\n", firsts[len(firsts)/2].Round(10*time.Microsecond))
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") {
				fmt.Println("  ", strings.Join(strings.Fields(l), " "))
			}
		}
	}
	return nil
}

type result struct {
	spawn, first, written, exit, trigger time.Duration
	triggerOK                            bool
}

func since(t0 int64) time.Duration { return time.Duration(clock.Mono() - t0) }

func playOnce(st *opusstream.Stream, data []byte, cfg sink.Config, prespawn bool, chunk int) (result, error) {
	var r result
	var p *sink.Player
	var err error
	if prespawn {
		if p, err = sink.Start(cfg); err != nil {
			return r, err
		}
		time.Sleep(300 * time.Millisecond) // aplay успевает открыть PCM и встать на чтение stdin
	}
	stop := make(chan struct{})
	t0 := clock.Mono()
	trig := watchTrigger(t0, stop)
	if !prespawn {
		if p, err = sink.Start(cfg); err != nil {
			close(stop)
			return r, err
		}
	}
	r.spawn = since(t0)

	st.Reset()
	first := true
	feed := func(pcmData []byte) error {
		if first {
			r.first, first = since(t0), false
		}
		_, err := p.Write(pcmData)
		return err
	}
	step := len(data)
	if chunk > 0 {
		step = chunk
	}
	var werr error
	for off := 0; off < len(data) && werr == nil; off += step {
		werr = st.Write(data[off:min(off+step, len(data))], feed)
	}
	r.written = since(t0)
	ferr := p.Finish(30 * time.Second)
	r.exit = since(t0)
	close(stop)
	if ns := <-trig; ns > 0 {
		r.trigger, r.triggerOK = time.Duration(ns-t0), true
	}
	if werr != nil {
		return r, werr
	}
	return r, ferr
}

// watchTrigger опрашивает статус PCM и возвращает trigger_time первого запуска новее since (0 — не было).
func watchTrigger(since int64, stop <-chan struct{}) <-chan int64 {
	ch := make(chan int64, 1)
	go func() {
		var buf [512]byte
		for {
			select {
			case <-stop:
				ch <- 0
				return
			default:
			}
			if state, trig, err := pcm.Read(buf[:]); err == nil && state == "RUNNING" && trig > since {
				ch <- trig
				return
			}
			time.Sleep(500 * time.Microsecond)
		}
	}()
	return ch
}

func decodeTiming(st *opusstream.Stream, data []byte) error {
	var n int
	run := func() (time.Duration, error) {
		st.Reset()
		t := time.Now()
		err := st.Write(data, func(b []byte) error { n += len(b); return nil })
		return time.Since(t), err
	}
	cold, err := run()
	if err != nil {
		return err
	}
	var warm []time.Duration
	for i := 0; i < 20; i++ {
		d, err := run()
		if err != nil {
			return err
		}
		warm = append(warm, d)
	}
	slices.Sort(warm)
	secs := float64(st.Samples()) / opusstream.SampleRate
	fmt.Printf("декод %.2f с звука: первый раз %v, потом медиана %v (%.0f× реального времени)\n", secs,
		cold.Round(10*time.Microsecond), warm[len(warm)/2].Round(10*time.Microsecond), secs/warm[len(warm)/2].Seconds())
	return nil
}
