package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"openmediad/internal/clock"
	"openmediad/internal/music"
	"openmediad/internal/obs"
	"openmediad/internal/player"
	"openmediad/internal/quasar"
	"openmediad/internal/shim"
	"openmediad/internal/wire"
)

// shimCmd — omd shim: прозрачный посредник перед сервисом шины (по умолчанию audioclient 9906).
// Клиенты попадают к нему правилом iptables REDIRECT (scripts/shim-on.sh); к настоящему сервису шим
// подключается с адреса 127.0.0.2, чтобы то же правило не заворачивало его самого.
func shimCmd(args []string) error {
	fs := flag.NewFlagSet("shim", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:19906", "где слушать клиентов")
	upstream := fs.String("upstream", "127.0.0.1:9906", "настоящий сервис")
	src := fs.String("src", "127.0.0.2", "локальный адрес соединений к сервису (пусто — по умолчанию системы)")
	every := fs.Duration("stats", 30*time.Second, "период сводки по кадрам; 0 — без сводки")
	verbose := fs.Bool("v", false, "печатать каждый кадр, кроме событий плеера (направление, тип, размер, задержка шима)")
	mono := fs.Bool("mono", false, "в строках -v ставить монотонное время прихода кадра (для сверки с pcmwatch)")
	musicOn := fs.Bool("music", false, "фаза 3: музыку Яндекса (плеер audio) играть самим, события собирать самим; "+
		"если поток не открылся, команда уходит настоящему mediad")
	fs.Parse(args)

	var srcIP net.IP
	if *src != "" {
		if srcIP = net.ParseIP(*src); srcIP == nil {
			return fmt.Errorf("-src: не адрес: %q", *src)
		}
	}
	logf := func(format string, a ...any) {
		fmt.Printf("%s %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, a...))
	}

	var window, total shim.Stats
	p := &shim.Proxy{Listen: *listen, Upstream: *upstream, Src: srcIP, Logf: logf}
	p.Tap = func(c *shim.Conn, d shim.Dir, frame []byte, lat time.Duration) {
		window.Tap(c, d, frame, lat)
		total.Tap(c, d, frame, lat)
		if *verbose {
			// События плеера идут десятками в секунду и забили бы журнал: печатаем всё остальное.
			if kind := shim.KindOf(frame); kind != "AudioEvent" {
				at := ""
				if *mono {
					at = fmt.Sprintf(" m%.4f", float64(clock.Mono()-lat.Nanoseconds())/1e9)
				}
				logf("conn %d %s %s%s %d Б, задержка шима %s", c.ID, d, kind, at, len(frame)+4, lat.Round(time.Microsecond))
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *musicOn {
		eng := attachMusic(p, logf)
		defer eng.Shutdown()
	}
	if *every > 0 {
		go func() {
			t := time.NewTicker(*every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					window.Report(os.Stdout, time.Now().UTC().Format("15:04:05.000")+" сводка за "+every.String(), true)
				}
			}
		}()
	}
	logf("шим: %s → %s (исходящие с %s)", *listen, *upstream, *src)
	err := p.Run(ctx)
	total.Report(os.Stdout, time.Now().UTC().Format("15:04:05.000")+" итого за время работы", false)
	logf("шим остановлен")
	return err
}

// shimCheck — omd shimcheck: независимая проверка шима по записи tcpdump (scripts/shim-capture.sh).
func shimCheck(args []string) error {
	fs := flag.NewFlagSet("shimcheck", flag.ExitOnError)
	client := fs.Uint("client", 19906, "порт шима для клиентов")
	service := fs.Uint("service", 9906, "порт настоящего сервиса")
	src := fs.String("src", "127.0.0.2", "адрес, с которого шим ходит к сервису")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	ip := net.ParseIP(*src)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("-src: не адрес IPv4: %q", *src)
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	return obs.ShimCheck{ClientPort: uint16(*client), ServicePort: uint16(*service), ShimAddr: ip}.Check(f, os.Stdout)
}

// attachMusic подключает к шиму фазу 3: перехватывает команды аудио-плеера (prefetch, play, pause, release,
// crossfade, перемотка), играет музыку движком music, а события плеера рассылает клиентам сам. Всё остальное
// (голос Алисы, будильники, громкость, фокус) идёт настоящему mediad как раньше.
func attachMusic(p *shim.Proxy, logf func(string, ...any)) *music.Engine {
	eng := &music.Engine{Logf: logf}
	eng.Warm()
	m := &player.Machine{
		Emit: func(body []byte) { p.Broadcast(wire.Wrap(wire.TypeAudioEvent, body)) },
		Forward: func(ctx any, body []byte) {
			if c, ok := ctx.(*shim.Conn); ok {
				if err := p.ForwardTo(c, wire.Wrap(wire.TypeAudioCommand, body)); err != nil {
					logf("музыка: возврат команды mediad: %v", err)
				}
			}
		},
		Now:  time.Now,
		Mono: func() uint64 { return uint64(clock.Mono() / 1e6) },
		After: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
		Back: eng,
		Logf: logf,
	}
	eng.OnOpened = m.Opened
	eng.OnFailed = m.Failed
	eng.OnFinished = m.Finished
	p.Intercept = func(c *shim.Conn, frame []byte) bool {
		msg, err := wire.Unwrap(frame)
		if err != nil || msg.Type != wire.TypeAudioCommand {
			return false
		}
		cmd := quasar.AudioCommand(msg.Body)
		eaten := m.Handle(c, cmd)
		if eaten {
			logf("музыка: %s %s обработана шимом", cmd.Classify(), cmd.Player().Kind)
		}
		return eaten
	}
	logf("музыка: перехват включён (плеер audio → собственный движок, откат на mediad при ошибке потока)")
	return eng
}
