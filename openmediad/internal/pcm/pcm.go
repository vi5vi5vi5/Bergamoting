// Package pcm читает состояние воспроизводящего PCM-устройства колонки из /proc/asound.
//
// Поле trigger_time — момент последнего запуска или остановки потока в CLOCK_MONOTONIC. Пока PCM в состоянии
// RUNNING, это именно момент СТАРТА потока в железо; после остановки значение перезаписывается моментом
// остановки, поэтому читать его нужно во время игры.
package pcm

import (
	"bytes"
	"os"
)

// StatusPath — первое подустройство воспроизведения карты 0 (TDM-B → усилитель ACM8625).
const StatusPath = "/proc/asound/card0/pcm0p/sub0/status"

// ParseStatus разбирает содержимое status. trigger — наносекунды CLOCK_MONOTONIC (0, если поля нет).
func ParseStatus(b []byte) (state string, trigger int64) {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		switch {
		case bytes.HasPrefix(line, []byte("state:")):
			state = string(bytes.TrimSpace(line[len("state:"):]))
		case bytes.HasPrefix(line, []byte("trigger_time:")):
			trigger = parseSeconds(bytes.TrimSpace(line[len("trigger_time:"):]))
		case state == "" && len(bytes.TrimSpace(line)) > 0 && !bytes.Contains(line, []byte(":")):
			state = string(bytes.TrimSpace(line)) // устройство закрыто: в файле одно слово «closed»
		}
	}
	return state, trigger
}

// parseSeconds превращает «3632.295651396» в наносекунды.
func parseSeconds(b []byte) int64 {
	var sec, frac int64
	digits := 0
	i := 0
	for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
		sec = sec*10 + int64(b[i]-'0')
	}
	if i < len(b) && b[i] == '.' {
		for i++; i < len(b) && b[i] >= '0' && b[i] <= '9' && digits < 9; i++ {
			frac = frac*10 + int64(b[i]-'0')
			digits++
		}
	}
	for ; digits < 9; digits++ {
		frac *= 10
	}
	return sec*1e9 + frac
}

// Read читает статус в buf (достаточно 512 байт) без выделения памяти.
func Read(buf []byte) (state string, trigger int64, err error) {
	f, err := os.Open(StatusPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", 0, err
	}
	state, trigger = ParseStatus(buf[:n])
	return state, trigger, nil
}
