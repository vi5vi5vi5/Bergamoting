package player

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"openmediad/internal/obs"
	"openmediad/internal/pb"
	"openmediad/internal/quasar"
	"openmediad/internal/wire"
)

// plainSnapFrom вынимает из настоящего события голоса входные данные сборщика.
func plainSnapFrom(ev []byte) PlainSnap {
	var s PlainSnap
	s.Kind = quasar.AudioEvent(ev).Kind()
	s.State = quasar.AudioEvent(ev).State()
	d, _ := pb.Bytes(ev, 3)
	s.PosMs, _ = pb.Uint(d, 28)
	s.Idx, _ = pb.Uint(d, 12)
	s.Has6 = pb.Has(d, 6)
	s.Dur6, _ = pb.Uint(d, 6)
	s.Mono, _ = pb.Uint(d, 29)
	s.UUID, _ = pb.Str(d, 22)
	s.Start, _ = pb.Uint(ev, 5)
	s.Stop, _ = pb.Uint(ev, 7)
	if v, ok := pb.Uint(ev, 9); ok {
		s.HasT9, s.T9 = true, v == 1
	}
	if v, ok := pb.Uint(ev, 17); ok {
		s.HasT17, s.T17 = true, v == 1
	}
	r := pb.NewReader(ev)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		if f.Num != 14 {
			continue
		}
		k, _ := pb.Str(f.B, 1)
		switch k {
		case "mark":
			s.Mark, _ = pb.Str(f.B, 2)
		case "vsid":
			s.Vsid, _ = pb.Str(f.B, 2)
		case "rss_mem":
			s.RSS, _ = pb.Uint(f.B, 3)
		case "bitrate":
			s.Bitrate, _ = pb.Uint(f.B, 3)
		case "played_sec":
			s.PlayedSec, _ = pb.Uint(f.B, 3)
			s.HasPlayed = true
		}
	}
	return s
}

// TestVoiceEventsMatchRecordings: событие голоса собирается из команды voice-start и входов (позиция, uuid, метки, diag)
// байт в байт. Свои события шима (rss_mem = 33000) пропускаются.
func TestVoiceEventsMatchRecordings(t *testing.T) {
	dir := capturesDir(t)
	files, _ := filepath.Glob(filepath.Join(dir, "*.pcap"))
	total, bad := 0, map[string]int{}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		starts := map[string]quasar.AudioCommand{}
		err = obs.WalkPcap(f, func(fr obs.Frame) {
			if fr.Svc != "audioclient" {
				return
			}
			m, err := wire.Unwrap(fr.Data)
			if err != nil {
				return
			}
			switch m.Type {
			case wire.TypeAudioCommand:
				c := quasar.AudioCommand(append([]byte(nil), m.Body...))
				if c.Classify() == quasar.CmdVoiceStart {
					starts[c.Player().ID] = c
				}
			case wire.TypeAudioEvent:
				e := quasar.AudioEvent(m.Body)
				p := e.Player()
				if p.Kind != quasar.PlayerVoice || e.Kind() == 5 || isSynthetic(m.Body) {
					return
				}
				c, ok := starts[p.ID]
				if !ok {
					return
				}
				sn := plainSnapFrom(m.Body)
				// правила, по которым автомат решает, что класть в событие (их голый «вход» сверка выше не проверяет):
				if sn.HasPlayed != (sn.PosMs >= 1000) {
					bad["played_sec присутствует не так, как «pos ≥ 1000»"]++
				}
				if sn.Idx != sn.PosMs/1000 {
					bad["секунды в поле 12 не равны pos/1000"]++
				}
				// 6 и 29 есть у ENDED и у progress/flag в состоянии PLAYING; нет у STARTING, PLAYING-как-смены-состояния и у flag в STARTING
				if sn.Has6 != (sn.State == 5 || (sn.Kind != 1 && sn.State == 3)) {
					bad[fmt.Sprintf("поле 6 не так: вид %v состояние %v есть=%v", e.Kind(), e.State(), sn.Has6)]++
				}
				got := PlainMetaFrom(c).Event(sn)
				total++
				if !bytes.Equal(got, m.Body) {
					bad[fmt.Sprintf("%v/%v %s", e.Kind(), e.State(), firstDiff(got, m.Body, ""))]++
				}
			}
		})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if total == 0 {
		t.Fatal("в записях нет событий голоса — проверка вхолостую")
	}
	t.Logf("событий голоса сверено: %d, не совпало: %d", total, sum(bad))
	var keys []string
	for k := range bad {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Errorf("расхождение %-40s × %d", k, bad[k])
	}
}

// TestFileEventsMatchRecordings: события плеера файлов (звонок таймера и будильника) собираются из стартовой команды и входов
// байт в байт; заодно проверяются правила наличия полей, по которым автомат решает, что класть в событие.
func TestFileEventsMatchRecordings(t *testing.T) {
	dir := capturesDir(t)
	files, _ := filepath.Glob(filepath.Join(dir, "*.pcap"))
	total, releases, bad := 0, 0, map[string]int{}
	stats := map[string]int{}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		starts := map[string]quasar.AudioCommand{}
		afterPlaying := map[string]bool{}                             // следующий progress в PLAYING у этого плеера — первый
		prevBody, prevFirst := map[string][]byte{}, map[string]bool{} // платформа рассылает событие по четырём соединениям: копии идут подряд
		err = obs.WalkPcap(f, func(fr obs.Frame) {
			if fr.Svc != "audioclient" {
				return
			}
			m, err := wire.Unwrap(fr.Data)
			if err != nil {
				return
			}
			switch m.Type {
			case wire.TypeAudioCommand:
				c := quasar.AudioCommand(append([]byte(nil), m.Body...))
				if c.Classify() == quasar.CmdFileStart {
					starts[c.Player().ID] = c
				}
				if c.Player().Kind == quasar.PlayerFile && c.Classify() == quasar.CmdRelease {
					releases++ // автомат ловит release звонка по этому же признаку
				}
			case wire.TypeAudioEvent:
				e := quasar.AudioEvent(m.Body)
				p := e.Player()
				if p.Kind != quasar.PlayerFile || e.Kind() == 5 || isSynthetic(m.Body) {
					return
				}
				c, ok := starts[p.ID]
				if !ok {
					return
				}
				sn := plainSnapFrom(m.Body)
				stats[fmt.Sprintf("%v/%v 6=%v(%d) 29=%v bitrate=%d played=%v t9=%v/%v t17=%v/%v", e.Kind(), e.State(), sn.Has6, sn.Dur6, sn.Mono > 0, sn.Bitrate, sn.HasPlayed, sn.HasT9, sn.T9, sn.HasT17, sn.T17)]++
				got := PlainMetaFrom(c).Event(sn)
				total++
				if !bytes.Equal(got, m.Body) {
					bad[fmt.Sprintf("%v/%v %s", e.Kind(), e.State(), firstDiff(got, m.Body, ""))]++
				}
				// таблица правил, по которой события строит автомат (file_machine.go), должна сходиться с настоящими
				first := prevFirst[p.ID]
				if !bytes.Equal(prevBody[p.ID], m.Body) {
					first = e.Kind() == 2 && e.State() == 3 && afterPlaying[p.ID]
					if e.Kind() == 1 && e.State() == 3 {
						afterPlaying[p.ID] = true
					} else if e.Kind() == 2 && e.State() == 3 {
						afterPlaying[p.ID] = false
					}
					prevBody[p.ID], prevFirst[p.ID] = append([]byte(nil), m.Body...), first
				}
				if v := fileRuleViolation(sn, first); v != "" {
					bad["правило: "+v]++
				}
			}
		})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if total == 0 {
		t.Fatal("в записях нет событий плеера файлов — проверка вхолостую")
	}
	if releases == 0 {
		t.Fatal("в записях нет команд release плеера файлов: не проверено, что Classify их узнаёт")
	}
	t.Logf("событий плеера файлов сверено: %d, не совпало: %d", total, sum(bad))
	var ks []string
	for k := range stats {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		t.Logf("  %-70s × %d", k, stats[k])
	}
	var keys []string
	for k := range bad {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Errorf("расхождение %-40s × %d", k, bad[k])
	}
}
