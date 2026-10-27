package main

import (
	"flag"
	"fmt"
	"os"

	"openmediad/internal/obs"
	"openmediad/internal/pb"
	"openmediad/internal/quasar"
	"openmediad/internal/wire"
)

// timelineCmd — omd timeline: сжатая шкала команд и событий плеера по записи tcpdump. Показывает то, что видела платформа:
// для записи со стороны шима берётся клиентская сторона (127.0.0.1 ↔ шим на 19906), для обычной записи — прямое соединение.
// Рассылка событий на четыре соединения платформы схлопывается в одну строку.
func timelineCmd(args []string) error {
	fs := flag.NewFlagSet("timeline", flag.ExitOnError)
	progress := fs.Bool("progress", false, "показывать события прогресса (раз в секунду)")
	voice := fs.Bool("voice", false, "показывать голосовой плеер Алисы")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("нужен один файл .pcap")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	obs.Ports[19906] = "audioclient" // клиентская сторона шима

	last := map[string]int64{}
	var t0 int64
	return obs.WalkPcap(f, func(fr obs.Frame) {
		if fr.Svc != "audioclient" || fr.ClientIP != [4]byte{127, 0, 0, 1} { // только то, что видит платформа
			return
		}
		m, err := wire.Unwrap(fr.Data)
		if err != nil {
			return
		}
		ms := fr.Time.UnixMilli()
		if t0 == 0 {
			t0 = ms
		}
		rel := float64(ms-t0) / 1000
		switch m.Type {
		case wire.TypeAudioCommand:
			c := quasar.AudioCommand(m.Body)
			p := c.Player()
			k := c.Classify()
			if p.Kind == quasar.PlayerVoice && !*voice || k == quasar.CmdOther && p.ID == "" || k == quasar.CmdEqualizer {
				return
			}
			if k == quasar.CmdFocus {
				v, _ := pb.Uint(c, 39)
				fmt.Printf("%9.3f КОМАНДА focus %d\n", rel, v)
				return
			}
			fmt.Printf("%9.3f КОМАНДА %-11s %s/%s\n", rel, k, p.Kind, short(p.ID))
		case wire.TypeAudioEvent:
			e := quasar.AudioEvent(m.Body)
			p := e.Player()
			if p.Kind == quasar.PlayerVoice && !*voice || e.Kind() == 5 || (e.Kind() == 2 && !*progress) {
				return
			}
			pos, _ := e.PositionMs()
			key := fmt.Sprintf("%v/%v/%s/%d", e.Kind(), e.State(), p.ID, pos)
			if l, ok := last[key]; ok && ms-l < 80 {
				return
			}
			last[key] = ms
			d, _ := pb.Bytes(m.Body, 3)
			dur, _ := pb.Uint(d, 6)
			fmt.Printf("%9.3f   событие %-8v %-8v %s/%s позиция %d мс, длительность %d с\n", rel, e.Kind(), e.State(), p.Kind, short(p.ID), pos, dur)
		}
	})
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
