package sound

import "strings"

// SoundsDir — где лежат системные звуки колонки (таймер, будильник, сигналы).
const SoundsDir = "/system/vendor/quasar/sounds/"

// TimerSound — звонок таймера и будильника по умолчанию (по имени «wav», по содержимому MP3 64 кбит/с).
const TimerSound = SoundsDir + "common/timer.wav"

// MaindPath — штатный maind колонки, в котором живёт GStreamer (то же, что music.MaindPath).
const MaindPath = "/system/vendor/quasar/maind"

// ValidPath сообщает, можно ли играть этот файл: только внутри SoundsDir и только из безопасных символов, потому что путь
// подставляется в строку запуска GStreamer.
func ValidPath(p string) bool {
	if !strings.HasPrefix(p, SoundsDir) || len(p) == len(SoundsDir) || strings.Contains(p, "..") {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-', r == '/':
		default:
			return false
		}
	}
	return true
}
