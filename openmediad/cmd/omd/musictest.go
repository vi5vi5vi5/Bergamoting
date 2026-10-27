package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"openmediad/internal/music"
	"openmediad/internal/player"
)

// musicTest — omd music-test: проигрывает один поток Яндекса движком music без шины и без Алисы.
// Нужен, чтобы проверить на колонке всю цепочку (HTTPS, AES, разбор MP4, GStreamer, aplay) отдельно от шима.
func musicTest(args []string) error {
	fs := flag.NewFlagSet("music-test", flag.ExitOnError)
	url := fs.String("url", "", "URL потока")
	key := fs.String("key", "", "ключ AES-128, 32 hex-символа (поле 39 команды)")
	from := fs.Uint64("from", 0, "начать с позиции, мс")
	dur := fs.Duration("for", 15*time.Second, "сколько играть")
	fs.Parse(args)
	k, err := hex.DecodeString(*key)
	if err != nil || len(k) != 16 || *url == "" {
		return fmt.Errorf("нужны -url и -key (32 hex-символа)")
	}
	t0 := time.Now()
	logf := func(f string, a ...any) {
		fmt.Printf("+%6.3fs %s\n", time.Since(t0).Seconds(), fmt.Sprintf(f, a...))
	}
	eng := &music.Engine{Logf: logf}
	opened := make(chan uint64, 1)
	failed := make(chan error, 1)
	finished := make(chan struct{}, 1)
	eng.OnOpened = func(_ string, d uint64) { opened <- d }
	eng.OnFailed = func(_ string, err error) { failed <- err }
	eng.OnFinished = func(string) { finished <- struct{}{} }
	defer eng.Shutdown()

	eng.Open("test", player.Track{ID: "test", URL: *url, Key: k, Title: "music-test"}, *from)
	select {
	case d := <-opened:
		logf("поток открыт: длительность %d мс, первые 300 мс звука готовы", d)
	case err := <-failed:
		return fmt.Errorf("поток не открылся: %w", err)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("за 30 с поток не открылся")
	}
	start := time.Now()
	eng.Start("test")
	end := time.After(*dur)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	first := false
	for {
		select {
		case <-tick.C:
			got, tot := eng.Net("test")
			logf("позиция %d мс, скачано %d из %d Б, RSS omd %d кБ", eng.Pos("test"), got, tot, rssKB())
		case <-finished:
			logf("трек доигран до конца")
			return nil
		case <-end:
			logf("время вышло (%s), останавливаю", time.Since(start).Round(time.Second))
			return nil
		}
		if !first && eng.Pos("test") > *from {
			first = true
			logf("звук пошёл через %s после Start", time.Since(start).Round(time.Millisecond))
		}
	}
}

func rssKB() int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	var kb int
	for _, l := range splitLines(string(b)) {
		if _, err := fmt.Sscanf(l, "VmRSS: %d kB", &kb); err == nil {
			return kb
		}
	}
	return 0
}

func splitLines(s string) (out []string) {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 10 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
