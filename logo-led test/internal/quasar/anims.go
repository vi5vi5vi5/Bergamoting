// Package quasar — наблюдение за прошивкой Алисы (Quasar).
package quasar

import (
	"bytes"
	"context"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// MaindLog — лог основного процесса прошивки; в него device_state_handler
// пишет каждое переключение анимаций подсветки.
const MaindLog = "/data/quasar/daemons_logs/maind.log"

// Anim — что прошивка сейчас показывает. Собирается из строки лога вида
//
//	Play animations: [ .../alice_listening.led ] [ .../alice_listening.binscled.zst ] source=onSDKState
//
// где первые скобки — анимации кольца, вторые — часов.
type Anim struct {
	Ring   []string
	Clock  []string
	Source string
	At     time.Time // когда строка появилась в логе (нулевое — найдена при старте)
}

// RingIdle — кольцо в покое: прошивка закончила на idle.led (всё погашено).
func (a Anim) RingIdle() bool {
	return len(a.Ring) == 0 || path.Base(a.Ring[len(a.Ring)-1]) == "idle.led"
}

// ClockIdle — часы в покое: показывают время («<Animation>» в логе).
func (a Anim) ClockIdle() bool {
	for _, c := range a.Clock {
		if c != "<Animation>" {
			return false
		}
	}
	return true
}

var playRe = regexp.MustCompile(`Play animations: \[ (.*?) \] \[ (.*?) \](?: source=(\S+))?`)

func parseLine(line []byte) (Anim, bool) {
	if !bytes.Contains(line, []byte("Play animations:")) {
		return Anim{}, false
	}
	m := playRe.FindSubmatch(line)
	if m == nil {
		return Anim{}, false
	}
	names := func(b []byte) []string {
		var out []string
		for _, f := range strings.Fields(string(b)) {
			out = append(out, path.Base(f))
		}
		return out
	}
	return Anim{Ring: names(m[1]), Clock: names(m[2]), Source: string(m[3])}, true
}

// WatchAnims опрашивает лог и вызывает onChange на каждое переключение анимаций.
// При старте один раз сообщает последнее известное состояние из уже записанного лога.
func WatchAnims(ctx context.Context, logPath string, every time.Duration, onChange func(Anim)) {
	t := &tail{path: logPath, off: -1}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		t.poll(onChange)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type tail struct {
	path    string
	off     int64 // сколько байт уже прочитано; -1 — ещё не читали
	partial []byte
}

// poll дочитывает новые строки. Файл открывается заново каждый раз: при ротации
// maind.log переименовывается в maind.log.1, и открытый дескриптор ушёл бы вместе с ним.
func (t *tail) poll(onChange func(Anim)) {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	initial := t.off < 0
	if initial || st.Size() < t.off {
		t.off, t.partial = 0, nil
	}
	if st.Size() == t.off {
		return
	}
	buf := make([]byte, st.Size()-t.off)
	n, _ := f.ReadAt(buf, t.off)
	t.off += int64(n)
	data := append(t.partial, buf[:n]...)
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		t.partial = data
		return
	}
	t.partial = append([]byte(nil), data[end+1:]...)

	var last Anim
	found := false
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		a, ok := parseLine(line)
		if !ok {
			continue
		}
		if initial {
			last, found = a, true
			continue
		}
		a.At = time.Now()
		onChange(a)
	}
	if found {
		onChange(last)
	}
}
