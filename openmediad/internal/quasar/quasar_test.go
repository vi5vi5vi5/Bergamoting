package quasar

import (
	"strings"
	"testing"

	"openmediad/internal/pb"
)

// Кадры собираются по структуре, подтверждённой на живом трафике Мини 3 (см. PROTOCOL.md §4–§5).
// Реальные кадры с токенами и идентификаторами аккаунта в репозиторий не кладём.

const reqID = "e681fc70-6d78-483a-b24c-df992ed6b153" // 36 символов

func asr(text string, final, flag uint64) []byte {
	var b []byte
	b = pb.AppendString(b, 1, text)
	b = pb.AppendUint(b, 2, final)
	b = pb.AppendString(b, 3, "")
	return pb.AppendUint(b, 4, flag)
}

func aliceState(kind uint64, text, req string, asrBody []byte) AliceState {
	var b []byte
	b = pb.AppendUint(b, 1, kind)
	if text != "" {
		b = pb.AppendString(b, 2, text)
	}
	b = pb.AppendUint(b, 5, 1)
	if req != "" {
		b = pb.AppendString(b, 7, req)
	}
	if asrBody != nil {
		b = pb.AppendBytes(b, 15, asrBody)
	}
	return b
}

func TestFinalPhrase(t *testing.T) {
	ok := aliceState(1, "Включи музыку", reqID, asr("Включи музыку", 1, 1))
	if id, text, found := ok.FinalPhrase(); !found || id != reqID || text != "Включи музыку" {
		t.Fatalf("финальная фраза в LISTENING: %q %q %v", id, text, found)
	}
	if _, _, found := aliceState(3, "Включи музыку", reqID, asr("Включи музыку", 1, 1)).FinalPhrase(); !found {
		t.Error("финальная фраза в BUSY тоже годится")
	}

	cases := map[string]AliceState{
		"состояние IDLE":      aliceState(0, "Включи музыку", reqID, asr("Включи музыку", 1, 1)),
		"состояние SPEAKING":  aliceState(2, "Включи музыку", reqID, asr("Включи музыку", 1, 1)),
		"не окончательная":    aliceState(1, "Включи", reqID, asr("Включи", 0, 1)),
		"нет флага 15.4":      aliceState(1, "Включи музыку", reqID, asr("Включи музыку", 1, 0)),
		"текст не совпадает":  aliceState(1, "Включи музыку", reqID, asr("Включи радио", 1, 1)),
		"пустой текст":        aliceState(1, "", reqID, asr("", 1, 1)),
		"короткий request id": aliceState(1, "Пауза", "abc", asr("Пауза", 1, 1)),
		"нет ASR":             aliceState(1, "Пауза", reqID, nil),
	}
	for name, s := range cases {
		if _, _, found := s.FinalPhrase(); found {
			t.Errorf("%s: фраза не должна считаться финальной", name)
		}
	}
	long := strings.Repeat("а", 300) // 600 байт UTF-8 > 512
	if _, _, found := aliceState(1, long, reqID, asr(long, 1, 1)).FinalPhrase(); found {
		t.Error("слишком длинная фраза должна отбрасываться")
	}
}

func TestAliceStateKinds(t *testing.T) {
	for k, want := range map[AliceStateKind]string{0: "IDLE", 1: "LISTENING", 2: "SPEAKING", 3: "BUSY", 5: "SHAZAM", 6: "NONE", 9: "state?"} {
		if k.String() != want {
			t.Errorf("%d → %s", k, k)
		}
	}
	for k, want := range map[AliceStateKind]bool{Idle: false, None: false, Listening: true, Speaking: true, Busy: true, Shazam: true} {
		if k.Active() != want {
			t.Errorf("%v.Active() = %v", k, k.Active())
		}
	}
}

func TestReply(t *testing.T) {
	reply := pb.AppendString(nil, 1, "Включаю.")
	reply = pb.AppendBytes(reply, 5, []byte(`{"response":{"directives":[{"name":"audio_play"}]}}`))
	s := AliceState(pb.AppendBytes(aliceState(3, "Включи музыку", reqID, nil), 3, reply))
	speech, js, ok := s.Reply()
	if !ok || speech != "Включаю." || !strings.Contains(string(js), "audio_play") {
		t.Fatalf("%q %q %v", speech, js, ok)
	}
	if _, _, ok := aliceState(1, "", reqID, nil).Reply(); ok {
		t.Error("ответа ещё нет")
	}
}

func player(kind uint64, id, stream string) []byte {
	var b []byte
	b = pb.AppendUint(b, 1, kind)
	b = pb.AppendString(b, 2, id)
	if stream != "" {
		b = pb.AppendString(b, 3, stream)
	}
	return pb.AppendUint(b, 4, 0)
}

func TestAudioEvent(t *testing.T) {
	track := pb.AppendString(nil, 1, "44683635")
	track = pb.AppendUint(track, 28, 12345)
	var b []byte
	b = pb.AppendUint(b, 1, 2)
	b = pb.AppendUint(b, 2, 3)
	b = pb.AppendBytes(b, 3, track)
	b = pb.AppendBytes(b, 4, player(1, "76006c91-aaaa", "44683635"))
	e := AudioEvent(b)

	if e.Kind() != 2 || e.Kind().String() != "progress" || e.State() != 3 || e.State().String() != "PLAYING" {
		t.Errorf("%v %v", e.Kind(), e.State())
	}
	if p := e.Player(); p.Kind != PlayerAudio || p.ID != "76006c91-aaaa" || p.Stream != "44683635" {
		t.Errorf("%+v", p)
	}
	if e.TrackID() != "44683635" {
		t.Error(e.TrackID())
	}
	if pos, ok := e.PositionMs(); !ok || pos != 12345 {
		t.Errorf("%d %v", pos, ok)
	}
	if _, ok := AudioEvent(pb.AppendUint(nil, 1, 1)).PositionMs(); ok {
		t.Error("у события без трека нет позиции")
	}
}

func trackDesc(id, url, sub string) []byte {
	var b []byte
	b = pb.AppendString(b, 1, id)
	b = pb.AppendString(b, 2, url)
	b = pb.AppendUint(b, 3, 38145)
	b = pb.AppendBytes(b, 8, pb.AppendString(pb.AppendString(nil, 1, reqID), 2, sub))
	md := pb.AppendString(pb.AppendString(nil, 1, "Signs"), 2, "Ikon")
	return pb.AppendBytes(b, 9, md)
}

func cmd(parts ...[]byte) AudioCommand {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func TestClassify(t *testing.T) {
	voice := pb.AppendBytes(nil, 22, player(2, "72daa5dc", ""))
	audio := pb.AppendBytes(nil, 22, player(1, "76006c91", "44683635"))
	file := pb.AppendBytes(nil, 22, pb.AppendUint(pb.AppendString(pb.AppendUint(nil, 1, 4), 2, "f1e2d3c4"), 4, 1))
	// дескриптор плеера файлов: {4:4 19:0 25:путь 50:0} (50 внутри дескриптора — не команда кроссфейда)
	fileDesc := func(path string) []byte {
		d := pb.AppendUint(pb.AppendUint(nil, 4, 4), 19, 0)
		if path != "" {
			d = pb.AppendString(d, 25, path)
		}
		return pb.AppendUint(d, 50, 0)
	}
	url := "https://strm.example.net/track"
	cases := []struct {
		name string
		c    AudioCommand
		want CommandKind
	}{
		{"голос: чанк Opus", cmd(voice, pb.AppendBytes(nil, 29, []byte("OggS"))), CmdVoiceChunk},
		{"голос: конец данных", cmd(voice, pb.AppendString(nil, 30, "")), CmdVoiceEOS},
		{"освобождение плеера", cmd(voice, pb.AppendString(nil, 31, "")), CmdRelease},
		{"голос: старт", cmd(pb.AppendBytes(nil, 1, trackDesc("", "", "tts_play_placeholder")), voice), CmdVoiceStart},
		{"музыка: старт", cmd(pb.AppendBytes(nil, 1, trackDesc("44683635", url, "music")), audio,
			pb.AppendString(nil, 52, "AUDIO_PLAY directive")), CmdPlay},
		{"музыка: предзагрузка", cmd(pb.AppendBytes(nil, 1, trackDesc("62744907", url, "music")), audio,
			pb.AppendString(nil, 52, "prefetchDirective: for prefetch")), CmdPrefetch},
		{"пауза", cmd(pb.AppendString(nil, 4, ""), audio, pb.AppendString(nil, 52, "completeDirective: PAUSE branch")), CmdPause},
		{"кроссфейд", cmd(audio, pb.AppendBytes(nil, 50, pb.AppendUint(nil, 1, 244100))), CmdCrossfade},
		{"эквалайзер", cmd(pb.AppendBytes(nil, 35, pb.AppendUint(nil, 2, 1)), pb.AppendString(nil, 52, "onEqualizerConfig")), CmdEqualizer},
		{"фокус", cmd(pb.AppendUint(nil, 39, 1)), CmdFocus},
		{"файл: старт звонка", cmd(pb.AppendBytes(nil, 1, fileDesc("/system/vendor/quasar/sounds/common/timer.wav")), file), CmdFileStart},
		{"файл: release звонка", cmd(file, pb.AppendString(nil, 31, "")), CmdRelease},
		{"файл: без пути — не старт", cmd(pb.AppendBytes(nil, 1, fileDesc("")), file), CmdOther},
		{"путь у плеера музыки — не файл", cmd(pb.AppendBytes(nil, 1, fileDesc("/x.wav")), audio), CmdOther},
		{"прочее", cmd(pb.AppendUint(nil, 77, 1)), CmdOther},
	}
	for _, c := range cases {
		if got := c.c.Classify(); got != c.want {
			t.Errorf("%s: %v, ждали %v", c.name, got, c.want)
		}
	}
}

func TestCommandTrackAndChunk(t *testing.T) {
	c := cmd(pb.AppendBytes(nil, 1, trackDesc("44683635", "https://strm.example.net/t", "music")),
		pb.AppendBytes(nil, 29, []byte("OggSdata")))
	tr, ok := c.Track()
	if !ok || tr.ID != "44683635" || tr.OffsetMs != 38145 || tr.SubName != "music" || tr.Title != "Signs" || tr.Artist != "Ikon" {
		t.Fatalf("%+v %v", tr, ok)
	}
	if chunk, ok := c.VoiceChunk(); !ok || string(chunk) != "OggSdata" {
		t.Errorf("%q %v", chunk, ok)
	}
}

func TestRemoting(t *testing.T) {
	r := Remoting(pb.AppendBytes(pb.AppendString(nil, 7, "AliceCapability"), 8, pb.AppendUint(nil, 1, 3)))
	if r.Object() != "AliceCapability" {
		t.Error(r.Object())
	}
	if m := r.Methods(); len(m) != 1 || m[0] != 8 {
		t.Errorf("%v", m)
	}
}

func TestHandshake(t *testing.T) {
	dev := pb.AppendString(nil, 1, "aliced")
	dev = pb.AppendString(dev, 2, "1.0")
	dev = pb.AppendString(dev, 6, "SERIAL")
	dev = pb.AppendString(dev, 11, "bergamot")
	var inner []byte
	inner = pb.AppendBytes(inner, 1, dev)
	inner = pb.AppendString(inner, 2, "audio_client")
	inner = pb.AppendString(inner, 2, "directive_sequencer")
	h := Handshake(pb.AppendBytes(nil, 1, inner))

	app, ver, serial, model := h.Device()
	if app != "aliced" || ver != "1.0" || serial != "SERIAL" || model != "bergamot" {
		t.Errorf("%q %q %q %q", app, ver, serial, model)
	}
	if f := h.Features(); len(f) != 2 || f[0] != "audio_client" || f[1] != "directive_sequencer" {
		t.Errorf("%v", f)
	}
}
