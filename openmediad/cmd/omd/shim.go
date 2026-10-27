package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"openmediad/internal/clock"
	"openmediad/internal/dbg"
	"openmediad/internal/iohub"
	"openmediad/internal/music"
	"openmediad/internal/obs"
	"openmediad/internal/player"
	"openmediad/internal/quasar"
	"openmediad/internal/shim"
	"openmediad/internal/sound"
	"openmediad/internal/voice"
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
	voiceOn := fs.Bool("voice", false, "фаза 5: голос Алисы (voice-start/chunk/eos) играть самим: Opus → aplay, события и спектр "+
		"собирать самим; если не заиграл за 3 с, весь ответ уходит настоящему mediad")
	voiceAplay := fs.String("voice-aplay", "", "-voice: путь к aplay для голоса (только для проверки отката: несуществующий путь заставляет mediad говорить сам)")
	soundsOn := fs.Bool("sounds", false, "фаза 5: звонки таймеров и будильников и системные сигналы (плеер файлов) играть самим: файл → PCM → "+
		"aplay по кругу до release; если не заиграл за 3 с, команда уходит настоящему mediad")
	soundsAplay := fs.String("sounds-aplay", "", "-sounds: путь к aplay для звонков (только для проверки отката: несуществующий путь заставляет mediad звонить сам)")
	spectrumOn := fs.Bool("spectrum", true, "-music и -voice: считать спектр для дисплея и светодиодов и отправлять его iohub (как штатный mediad)")
	debugAddr := fs.String("debug", "", "отладочный порт, например 0.0.0.0:6701 (пусто — выключен): веб-страница и журнал "+
		"с пояснениями, только чтение; на странице видны распознанные фразы и названия треков, но не ключи и не ссылки")
	debugCORS := fs.Bool("debug-cors", false, "-debug: разрешить чтение журнала странице с другого источника (локальный html-файл)")
	fs.Parse(args)

	var srcIP net.IP
	if *src != "" {
		if srcIP = net.ParseIP(*src); srcIP == nil {
			return fmt.Errorf("-src: не адрес: %q", *src)
		}
	}
	var hub *dbg.Hub
	if *debugAddr != "" {
		hub = &dbg.Hub{}
	}
	logf := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		fmt.Printf("%s %s\n", time.Now().UTC().Format("15:04:05.000"), msg)
		if hub != nil {
			hub.Log(dbg.Info, "shim", msg, "", nil)
		}
	}

	var window, total shim.Stats
	var dup dedup
	p := &shim.Proxy{Listen: *listen, Upstream: *upstream, Src: srcIP, Logf: logf}
	p.Tap = func(c *shim.Conn, d shim.Dir, frame []byte, lat time.Duration) {
		window.Tap(c, d, frame, lat)
		total.Tap(c, d, frame, lat)
		// События плеера mediad рассылает всем четырём соединениям платформы: в журнале нужна одна копия.
		if hub != nil && !(d == shim.ToClient && dup.seen(frame)) {
			if e, ok := dbg.DescribeFrame(d == shim.ToService, frame); ok {
				if e.Data == nil {
					e.Data = map[string]any{}
				}
				e.Data["conn"] = c.ID
				hub.Log(e.Lvl, e.Src, e.Msg, e.Why, e.Data)
			}
		}
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mach *player.Machine
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		if mach != nil {
			mach.Handoff() // платформа должна узнать, что музыка стоит на паузе, пока соединения шима ещё открыты
			time.Sleep(150 * time.Millisecond)
		}
		cancel()
	}()
	var eng *music.Engine
	mode := ""
	if *musicOn || *voiceOn || *soundsOn {
		a := attachPlayer(p, logf, hub, playerOpts{Music: *musicOn, Voice: *voiceOn, Sounds: *soundsOn, VoiceAplay: *voiceAplay, SoundsAplay: *soundsAplay})
		eng, mach, mode = a.Music, a.Machine, a.mode()
		defer eng.Shutdown()
		if a.Voice != nil {
			defer a.Voice.Shutdown()
		}
		if a.Sounds != nil {
			defer a.Sounds.Shutdown()
		}
		if *spectrumOn {
			cli := &iohub.Client{Logf: logf}
			snd := newSpecSender(cli, specDelay, hub)
			if *musicOn {
				eng.OnSpectrum = snd.Add
			}
			if a.Voice != nil {
				a.Voice.OnSpectrum = snd.Add
			}
			go cli.Run(ctx)
			go snd.Run(ctx)
		}
	}
	if hub != nil {
		startDebug(ctx, *debugAddr, *debugCORS, hub, debugState(p, mach, eng, mode), logf)
		watchAliced(ctx, hub)
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

// playerOpts — что шим забирает у mediad.
type playerOpts struct {
	Music, Voice, Sounds    bool
	VoiceAplay, SoundsAplay string // пути к aplay: несуществующий путь заставляет mediad играть самим (проверка отката)
}

// attached — движки и автомат, подключённые к шиму. Voice и Sounds равны nil, если выключены.
type attached struct {
	Music   *music.Engine
	Voice   *voice.Engine
	Sounds  *sound.Engine
	Machine *player.Machine
	opts    playerOpts
}

// mode — подпись режима для отладочной страницы.
func (a attached) mode() string {
	var own []string
	if a.opts.Music {
		own = append(own, "музыку Яндекса (-music)")
	}
	if a.opts.Voice {
		own = append(own, "голос Алисы (-voice)")
	}
	if a.opts.Sounds {
		own = append(own, "звонки и системные звуки (-sounds)")
	}
	return "шим играет сам: " + strings.Join(own, ", ") + "; остальное играет mediad"
}

// attachPlayer подключает к шиму перехват плеера. С Music шим играет музыку Яндекса сам (команды аудио-плеера: prefetch, play,
// pause, release, crossfade, перемотка), с Voice — голос Алисы (voice-start, voice-chunk, voice-eos, release голоса), с Sounds —
// звонки таймеров и будильников и системные сигналы (плеер файлов: старт с путём и release); события плеера рассылает клиентам
// сам. Всё остальное (громкость, фокус, эквалайзер, незнакомое) идёт настоящему mediad.
func attachPlayer(p *shim.Proxy, logf func(string, ...any), hub *dbg.Hub, o playerOpts) attached {
	engLog, machLog, voiceLog, soundLog := logf, logf, logf, logf
	if hub != nil {
		engLog, machLog, voiceLog, soundLog = hub.Logf("music"), hub.Logf("player"), hub.Logf("voice"), hub.Logf("sound")
	}
	eng := &music.Engine{Logf: engLog}
	eng.Warm()
	m := &player.Machine{
		Emit: func(body []byte) {
			p.Broadcast(wire.Wrap(wire.TypeAudioEvent, body))
			if hub != nil {
				debugEvent(hub, body)
			}
		},
		Forward: func(ctx any, body []byte) {
			if c, ok := ctx.(*shim.Conn); ok {
				if err := p.ForwardTo(c, wire.Wrap(wire.TypeAudioCommand, body)); err != nil {
					logf("возврат команды mediad: %v", err)
				}
			}
		},
		Now:  time.Now,
		Mono: func() uint64 { return uint64(clock.Mono() / 1e6) },
		After: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
		Back:    eng,
		Logf:    machLog,
		NoMusic: !o.Music,
	}
	if hub != nil {
		m.Note = debugNotes(hub)
	}
	eng.OnOpened = m.Opened
	eng.OnFailed = m.Failed
	eng.OnFinished = m.Finished
	a := attached{Music: eng, Machine: m, opts: o}
	if o.Voice {
		a.Voice = &voice.Engine{Logf: voiceLog, AplayPath: o.VoiceAplay, OnPlaying: m.VoicePlaying, OnEnded: m.VoiceEnded, OnFailed: m.VoiceFailed}
		m.Voice = a.Voice
	}
	if o.Sounds {
		se := &sound.Engine{Logf: soundLog, AplayPath: o.SoundsAplay, OnPlaying: m.FilePlaying, OnFailed: m.FileFailed}
		a.Sounds, m.File, m.FileOK = se, se, sound.ValidPath
		go func() { // звонок таймера и будильника: раскодировать заранее, чтобы первый звук не ждал GStreamer
			if err := se.Preload(sound.TimerSound); err != nil {
				soundLog("прогрев %s не удался: %v (звонок раскодируется при первом запуске)", sound.TimerSound, err)
			} else {
				soundLog("прогрето в память: %s", sound.TimerSound)
			}
		}()
	}
	p.Intercept = func(c *shim.Conn, frame []byte) bool {
		msg, err := wire.Unwrap(frame)
		if err != nil || msg.Type != wire.TypeAudioCommand {
			return false
		}
		cmd := quasar.AudioCommand(msg.Body)
		eaten := m.Handle(c, cmd)
		if eaten {
			if hub != nil {
				debugIntercept(hub, c.ID, cmd)
			} else {
				logf("перехвачено: %s %s", cmd.Classify(), cmd.Player().Kind)
			}
		}
		return eaten
	}
	logf("перехват плеера включён: музыка %v, голос Алисы %v, звонки и системные звуки %v (откат на mediad при ошибке)", o.Music, o.Voice, o.Sounds)
	return a
}

// dedup узнаёт кадры, уже виденные за последние 300 мс (копии одной рассылки на другие соединения).
type dedup struct {
	mu   sync.Mutex
	last [32]struct {
		h uint64
		t time.Time
	}
	next int
}

func (d *dedup) seen(frame []byte) bool {
	h := fnv.New64a()
	h.Write(frame)
	sum := h.Sum64()
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.last {
		if e.h == sum && now.Sub(e.t) < 300*time.Millisecond {
			return true
		}
	}
	d.last[d.next] = struct {
		h uint64
		t time.Time
	}{sum, now}
	d.next = (d.next + 1) % len(d.last)
	return false
}
