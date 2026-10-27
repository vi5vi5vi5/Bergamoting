// omd — инструмент разработки openmediad. Фаза 0: наблюдение за шиной Quasar.
//
//	omd observe [-aliced 127.0.0.1:9875] [-audio 127.0.0.1:9906] [-progress] [-noisy] [-stats 10s]
//	    подключается к сервисам как обычный клиент и печатает их рассылки (ничего не отправляет);
//	omd pcap [-progress] [-noisy] [-dump [-dump-max 160]] [-voice-dir каталог] файл.pcap|-
//	    декодирует запись tcpdump -i lo обоих направлений (команды между сервисами видны только так);
//	    -voice-dir сохраняет голос Алисы файлами .ogg.
//
//	omd pcmwatch [-for 60s]
//	    печатает смены состояния PCM и trigger_time (запуск потока ядром): шкала времени для замера задержки;
//	omd voice-play [флаги] файл.ogg
//	    проигрывает чанк голоса связкой «Go-декодер → pipe → aplay» и мерит задержку до запуска PCM;
//	omd shim [-listen 127.0.0.1:19906] [-upstream 127.0.0.1:9906] [-src 127.0.0.2] [-stats 30s] [-v [-mono]] [-music]
//	    прозрачный посредник перед audioclient (фаза 2): кадры пересылаются как есть, ведётся сводка;
//	    -music (фаза 3): музыку Яндекса играет сам шим, откат на mediad при ошибке потока;
//	omd shimcheck [-client 19906] [-service 9906] [-src 127.0.0.2] файл.pcap
//	    по записи tcpdump обеих сторон шима сверяет кадры (хэши) и считает добавляемую шимом задержку.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"openmediad/internal/clock"
	"openmediad/internal/obs"
)

func main() {
	// Движок должен быть лёгким: частая сборка мусора дешевле лишних мегабайт RSS на колонке.
	debug.SetGCPercent(25)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "observe":
		err = observe(os.Args[2:])
	case "pcap":
		err = pcap(os.Args[2:])
	case "pcmwatch":
		err = pcmWatch(os.Args[2:])
	case "voice-play":
		err = voicePlay(os.Args[2:])
	case "shim":
		err = shimCmd(os.Args[2:])
	case "shimcheck":
		err = shimCheck(os.Args[2:])
	case "music-test":
		err = musicTest(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "omd:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `использование:
  omd observe [-aliced адрес] [-audio адрес] [-progress] [-noisy] [-stats 10s]
  omd pcap [-progress] [-noisy] [-dump [-dump-max N]] [-voice-dir каталог] файл.pcap|-
  omd pcmwatch [-for 60s]
  omd voice-play [-dev default] [-start-delay 1] [-prespawn] [-repeat N] файл.ogg   (только на колонке)
  omd shim [-listen 127.0.0.1:19906] [-upstream 127.0.0.1:9906] [-src 127.0.0.2] [-stats 30s] [-v [-mono]] [-music]
  omd shimcheck [-client 19906] [-service 9906] [-src 127.0.0.2] файл.pcap
  omd music-test -url URL -key HEX [-from мс] [-for 15s]   (на колонке: один поток Яндекса движком music)`)
}

func observe(args []string) error {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	aliced := fs.String("aliced", "127.0.0.1:9875", "адрес aliced (пусто — не подключаться)")
	audio := fs.String("audio", "127.0.0.1:9906", "адрес audioclient (пусто — не подключаться)")
	progress := fs.Bool("progress", false, "показывать события прогресса плеера")
	noisy := fs.Bool("noisy", false, "показывать фоновые сообщения")
	stats := fs.Duration("stats", 0, "период сводки по типам кадров, например 10s")
	mono := fs.Bool("mono", false, "добавлять к времени монотонную метку (для сопоставления с pcmwatch)")
	bind := fs.String("bind", "", "локальный адрес исходящих соединений, например 127.0.0.3")
	fs.Parse(args)

	o := &obs.Observer{
		Opt:   obs.Opt{Progress: *progress, Noisy: *noisy},
		Out:   os.Stdout,
		Stats: *stats,
	}
	if *mono {
		o.Mono = clock.Mono
	}
	if *bind != "" {
		if o.Bind = net.ParseIP(*bind); o.Bind == nil {
			return fmt.Errorf("-bind: не адрес: %q", *bind)
		}
	}
	if *aliced != "" {
		o.Targets = append(o.Targets, obs.Target{Name: "aliced", Addr: *aliced})
	}
	if *audio != "" {
		o.Targets = append(o.Targets, obs.Target{Name: "audioclient", Addr: *audio})
	}
	if len(o.Targets) == 0 {
		return fmt.Errorf("нечего наблюдать")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := o.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func pcap(args []string) error {
	fs := flag.NewFlagSet("pcap", flag.ExitOnError)
	progress := fs.Bool("progress", false, "показывать события прогресса плеера")
	noisy := fs.Bool("noisy", false, "показывать фоновые сообщения")
	voiceDir := fs.String("voice-dir", "", "сохранять голос Алисы (Ogg/Opus) файлами .ogg в этот каталог")
	dump := fs.Bool("dump", false, "после команд и событий плеера печатать дерево полей целиком")
	dumpMax := fs.Int("dump-max", 160, "-dump: обрезать значения длиннее стольких байт")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	if *voiceDir != "" {
		if err := os.MkdirAll(*voiceDir, 0o700); err != nil {
			return err
		}
	}
	in := os.Stdin
	if p := fs.Arg(0); p != "-" {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	return obs.DecodePcap(in, obs.Opt{Progress: *progress, Noisy: *noisy, VoiceDir: *voiceDir, Dump: *dump, DumpMax: *dumpMax}, os.Stdout)
}
