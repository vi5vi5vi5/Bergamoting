package quasar

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Строки взяты из живого maind.log колонки.
const (
	lineListening = `[1923:1948] [2026-10-26 19:21:02.816] [device_state_handler] [info] [device_state_handler.cpp:743] Play animations: [ /system/vendor/quasar/ledpatterns/oknix//alice_listening.led ] [ /system/vendor/quasar/ledpatterns/clock/alice_listening.binscled.zst ] source=onSDKState`
	lineFadeOut   = `[1923:1946] [2026-10-26 19:13:56.429] [device_state_handler] [info] [device_state_handler.cpp:743] Play animations: [ FadeOut /system/vendor/quasar/ledpatterns/oknix//intonation_spotting.led idle.led ] [ spectrogram ] source=onSDKState`
	lineIdle      = `[1923:1941] [2026-10-26 19:21:04.154] [device_state_handler] [info] [device_state_handler.cpp:743] Play animations: [ idle.led ] [ <Animation> ] source=onSDKState`
	lineOther     = `[1923:1957] [2026-10-26 19:21:04.128] [sdk] [info] [directive_sequencer.cpp:758] routeDirective (xx51c344d6)draw_animation_directive.activation via LedCapability`
)

func TestParseLine(t *testing.T) {
	a, ok := parseLine([]byte(lineListening))
	if !ok || !slices.Equal(a.Ring, []string{"alice_listening.led"}) ||
		!slices.Equal(a.Clock, []string{"alice_listening.binscled.zst"}) || a.Source != "onSDKState" {
		t.Fatalf("listening: %+v %v", a, ok)
	}
	if a.RingIdle() || a.ClockIdle() {
		t.Errorf("listening не должен считаться покоем")
	}

	a, _ = parseLine([]byte(lineFadeOut))
	if !a.RingIdle() || a.ClockIdle() {
		t.Errorf("fade-out в idle.led: кольцо в покое, часы заняты спектрограммой; got %+v", a)
	}

	a, _ = parseLine([]byte(lineIdle))
	if !a.RingIdle() || !a.ClockIdle() {
		t.Errorf("idle: %+v", a)
	}

	if _, ok := parseLine([]byte(lineOther)); ok {
		t.Errorf("чужая строка распознана как смена анимаций")
	}
}

func TestTailRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "maind.log")
	write := func(s string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString(s)
		f.Close()
	}
	var got []Anim
	collect := func(a Anim) { got = append(got, a) }
	tl := &tail{path: p, off: -1}

	write(lineListening + "\n" + lineOther + "\n" + lineIdle + "\n")
	tl.poll(collect)
	if len(got) != 1 || !got[0].RingIdle() || !got[0].At.IsZero() {
		t.Fatalf("при старте ждём одно последнее состояние: %+v", got)
	}

	write(lineListening[:40]) // строка дописана не до конца
	tl.poll(collect)
	write(lineListening[40:] + "\n")
	tl.poll(collect)
	if len(got) != 2 || got[1].RingIdle() || got[1].At.IsZero() {
		t.Fatalf("дописанная строка: %+v", got)
	}

	os.Rename(p, p+".1") // ротация
	write(lineIdle + "\n")
	tl.poll(collect)
	if len(got) != 3 || !got[2].RingIdle() {
		t.Fatalf("после ротации: %+v", got)
	}
}
