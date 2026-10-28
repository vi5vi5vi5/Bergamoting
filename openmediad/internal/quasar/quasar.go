// Package quasar — типизированные «окна» на сообщения шины. Это не полноценная схема: читаются только
// поля, подтверждённые на живом трафике (quasar messaging/PROTOCOL.md), всё остальное остаётся сырым.
// Имена, помеченные «вывод», восстановлены по поведению и могут уточниться.
package quasar

import (
	"strings"

	"openmediad/internal/pb"
)

// --- AliceState (тип 3, aliced) ---

// AliceStateKind — поле 1 AliceState. Значения подтверждены сверкой шины с логом maind
// (onAliceStateChanged): 0 IDLE, 1 LISTENING, 2 SPEAKING, 3 BUSY. 5 и 6 — по коду spotiya.
type AliceStateKind uint64

const (
	Idle      AliceStateKind = 0
	Listening AliceStateKind = 1
	Speaking  AliceStateKind = 2
	Busy      AliceStateKind = 3
	Shazam    AliceStateKind = 5
	None      AliceStateKind = 6
)

func (k AliceStateKind) String() string {
	switch k {
	case Idle:
		return "IDLE"
	case Listening:
		return "LISTENING"
	case Speaking:
		return "SPEAKING"
	case Busy:
		return "BUSY"
	case Shazam:
		return "SHAZAM"
	case None:
		return "NONE"
	}
	return "state?"
}

// Active — Алиса сейчас занята (слушает, думает, говорит): повод приглушить музыку.
func (k AliceStateKind) Active() bool {
	switch k {
	case Listening, Speaking, Busy, Shazam:
		return true
	}
	return false
}

type AliceState []byte

func (s AliceState) Kind() AliceStateKind {
	v, _ := pb.Uint(s, 1)
	return AliceStateKind(v)
}

// Text — распознанная фраза (поле 2); пока Алиса не услышала речь — пусто.
func (s AliceState) Text() string { t, _ := pb.Str(s, 2); return t }

// RequestID — UUID запроса (поле 7), сквозной идентификатор диалога.
func (s AliceState) RequestID() string { t, _ := pb.Str(s, 7); return t }

// Source — метка источника запроса (поле 10.6), например «activation».
func (s AliceState) Source() string {
	b, ok := pb.Bytes(s, 10)
	if !ok {
		return ""
	}
	t, _ := pb.Str(b, 6)
	return t
}

// ASR — подсостояние распознавания (поле 15): текст, окончательный ли он.
func (s AliceState) ASR() (text string, final bool, ok bool) {
	b, ok := pb.Bytes(s, 15)
	if !ok {
		return "", false, false
	}
	text, _ = pb.Str(b, 1)
	f, _ := pb.Uint(b, 2)
	return text, f == 1, text != ""
}

// FinalPhrase — окончательная фраза, пригодная к перехвату. Правило то же, что у spotiya:
// состояние LISTENING или BUSY, ASR помечен окончательным (15.2==1, 15.4==1), текст совпадает с полем 2,
// UUID запроса — 36 символов. Срабатывает примерно на секунду раньше, чем Алиса начнёт отвечать.
func (s AliceState) FinalPhrase() (reqID, text string, ok bool) {
	if k := s.Kind(); k != Listening && k != Busy {
		return "", "", false
	}
	asr, found := pb.Bytes(s, 15)
	if !found {
		return "", "", false
	}
	if f, _ := pb.Uint(asr, 2); f != 1 {
		return "", "", false
	}
	if v, _ := pb.Uint(asr, 4); v != 1 {
		return "", "", false
	}
	text, _ = pb.Str(asr, 1)
	if text == "" || len(text) > 512 || text != s.Text() {
		return "", "", false
	}
	reqID = s.RequestID()
	if len(reqID) != 36 {
		return "", "", false
	}
	return reqID, text, true
}

// Reply — ответ облака (поле 3): произносимый текст и JSON с директивами (поле 3.5).
func (s AliceState) Reply() (speech string, directivesJSON []byte, ok bool) {
	b, ok := pb.Bytes(s, 3)
	if !ok {
		return "", nil, false
	}
	speech, _ = pb.Str(b, 1)
	directivesJSON, _ = pb.Bytes(b, 5)
	return speech, directivesJSON, true
}

// --- AudioEvent (тип 127, mediad → platformd) ---

type PlayerKind uint64

const (
	PlayerAudio PlayerKind = 1 // музыка/потоки
	PlayerVoice PlayerKind = 2 // голос Алисы (TTS)
	PlayerFile  PlayerKind = 4 // воспроизведение локального файла: звонок таймера и будильника, системные звуки
)

func (k PlayerKind) String() string {
	switch k {
	case PlayerAudio:
		return "audio"
	case PlayerVoice:
		return "voice"
	case PlayerFile:
		return "file"
	}
	return "player?"
}

// Player — дескриптор плеера: 22 в команде, 4 в событии.
type Player struct {
	Kind   PlayerKind
	ID     string // UUID плеера
	Stream string // id трека (у голоса пусто)
}

func readPlayer(b []byte) Player {
	k, _ := pb.Uint(b, 1)
	id, _ := pb.Str(b, 2)
	st, _ := pb.Str(b, 3)
	return Player{Kind: PlayerKind(k), ID: id, Stream: st}
}

// PlayState — поле 2 AudioEvent (вывод по поведению: 1 старт, 3 играет, 4 пауза, 5 завершён).
type PlayState uint64

func (p PlayState) String() string {
	switch p {
	case 1:
		return "STARTING"
	case 3:
		return "PLAYING"
	case 4:
		return "PAUSED"
	case 5:
		return "ENDED"
	}
	return "state?"
}

// EventKind — поле 1 AudioEvent (вывод: 1 смена состояния, 2 прогресс ~1 Гц, 5 диагностика, 6 флаг фокуса).
type EventKind uint64

func (e EventKind) String() string {
	switch e {
	case 1:
		return "state"
	case 2:
		return "progress"
	case 5:
		return "diag"
	case 6:
		return "flag"
	}
	return "ev?"
}

type AudioEvent []byte

func (e AudioEvent) Kind() EventKind {
	v, _ := pb.Uint(e, 1)
	return EventKind(v)
}

func (e AudioEvent) State() PlayState {
	v, _ := pb.Uint(e, 2)
	return PlayState(v)
}

func (e AudioEvent) Player() Player {
	b, _ := pb.Bytes(e, 4)
	return readPlayer(b)
}

// TrackID и PositionMs берутся из описания трека (поле 3): 3.1 и 3.28.
func (e AudioEvent) TrackID() string {
	b, _ := pb.Bytes(e, 3)
	s, _ := pb.Str(b, 1)
	return s
}

func (e AudioEvent) PositionMs() (uint64, bool) {
	b, ok := pb.Bytes(e, 3)
	if !ok {
		return 0, false
	}
	return pb.Uint(b, 28)
}

// --- AudioCommand (тип 8, platformd → mediad) ---

type CommandKind int

const (
	CmdOther CommandKind = iota
	CmdVoiceStart
	CmdVoiceChunk
	CmdVoiceEOS
	CmdRelease
	CmdPlay
	CmdPrefetch
	CmdPause
	CmdFocus
	CmdEqualizer
	CmdCrossfade
	CmdFileStart // старт плеера файлов: поле 1.25 — путь к файлу
)

func (c CommandKind) String() string {
	return [...]string{"other", "voice-start", "voice-chunk", "voice-eos", "release", "play",
		"prefetch", "pause", "focus", "equalizer", "crossfade", "file-start"}[c]
}

type AudioCommand []byte

func (c AudioCommand) Player() Player {
	b, _ := pb.Bytes(c, 22)
	return readPlayer(b)
}

// Track — описание трека (поле 1): id, url, смещение, название, исполнитель.
type Track struct {
	ID       string
	URL      string
	OffsetMs uint64
	SubName  string // 1.8.2: «music» или «tts_play_placeholder»
	Title    string
	Artist   string
}

func (c AudioCommand) Track() (Track, bool) {
	b, ok := pb.Bytes(c, 1)
	if !ok {
		return Track{}, false
	}
	var t Track
	t.ID, _ = pb.Str(b, 1)
	t.URL, _ = pb.Str(b, 2)
	t.OffsetMs, _ = pb.Uint(b, 3)
	if ids, ok := pb.Bytes(b, 8); ok {
		t.SubName, _ = pb.Str(ids, 2)
	}
	if md, ok := pb.Bytes(b, 9); ok {
		t.Title, _ = pb.Str(md, 1)
		t.Artist, _ = pb.Str(md, 2)
	}
	return t, true
}

// Reason — отладочная строка причины (поле 52), её прошивка ставит сама: «AUDIO_PLAY directive» и т. п.
func (c AudioCommand) Reason() string { s, _ := pb.Str(c, 52); return s }

// VoiceChunk — Ogg/Opus-кадры голоса Алисы (поле 29). Именно их надо декодировать,
// чтобы вывести ответ Алисы куда-то кроме штатного динамика.
func (c AudioCommand) VoiceChunk() ([]byte, bool) { return pb.Bytes(c, 29) }

// Classify грубо определяет назначение команды. Правила выведены из записи (вывод), порядок важен.
func (c AudioCommand) Classify() CommandKind {
	p := c.Player()
	switch {
	case pb.Has(c, 29):
		return CmdVoiceChunk
	case pb.Has(c, 30):
		return CmdVoiceEOS
	case pb.Has(c, 31):
		return CmdRelease
	case pb.Has(c, 4):
		return CmdPause
	case pb.Has(c, 50):
		return CmdCrossfade
	case pb.Has(c, 35):
		return CmdEqualizer
	}
	if t, ok := c.Track(); ok {
		switch {
		case p.Kind == PlayerVoice || t.SubName == "tts_play_placeholder":
			return CmdVoiceStart
		case t.URL != "" && strings.Contains(c.Reason(), "prefetch"):
			return CmdPrefetch
		case t.URL != "":
			return CmdPlay
		}
	}
	if p.Kind == PlayerFile {
		if _, ok := c.FilePath(); ok {
			return CmdFileStart
		}
	}
	if pb.Has(c, 39) {
		return CmdFocus
	}
	return CmdOther
}

// FileLoops — звучит ли файл по кругу, пока платформа не пришлёт release: поле 1.50 дескриптора равно 0.
// У звонка таймера и будильника оно 0 (timer.wav, 50:0); у системных звуков (звук после «Алиса», щелчок громкости:
// spotter_start.mp3, volume_sound.mp3) оно 1: файл играет один раз, и конец сообщает сам плеер. Если поля нет, считаем,
// что файл одноразовый: по кругу играем только то, что видели в записях.
func (c AudioCommand) FileLoops() bool {
	d, ok := pb.Bytes(c, 1)
	if !ok {
		return false
	}
	v, ok := pb.Uint(d, 50)
	return ok && v == 0
}

// FilePath — путь к файлу из стартовой команды плеера файлов (поле 1.25).
func (c AudioCommand) FilePath() (string, bool) {
	d, ok := pb.Bytes(c, 1)
	if !ok {
		return "", false
	}
	p, ok := pb.Str(d, 25)
	return p, ok && p != ""
}

// --- Remoting (тип 210, aliced) ---

type Remoting []byte

// Object — имя капабилити (поле 7): «AliceCapability», «PlaybackControlCapability» и т. д.
func (r Remoting) Object() string { s, _ := pb.Str(r, 7); return s }

// Methods — номера полей-методов (всё, кроме поля 7).
func (r Remoting) Methods() []uint64 {
	var out []uint64
	for _, n := range pb.Nums(r) {
		if n != 7 {
			out = append(out, n)
		}
	}
	return out
}

// --- Handshake (тип 207) ---

// Handshake — первое сообщение при подключении к aliced: идентификация устройства и список возможностей.
type Handshake []byte

func (h Handshake) Device() (appID, version, serial, model string) {
	d, _ := pb.Bytes(h, 1)
	d, _ = pb.Bytes(d, 1)
	appID, _ = pb.Str(d, 1)
	version, _ = pb.Str(d, 2)
	serial, _ = pb.Str(d, 6)
	model, _ = pb.Str(d, 11)
	return
}

func (h Handshake) Features() []string {
	d, _ := pb.Bytes(h, 1)
	var out []string
	r := pb.NewReader(d)
	for {
		f, err := r.Next()
		if err != nil {
			return out
		}
		if f.Num == 2 && f.Wire == pb.WireBytes {
			out = append(out, string(f.B))
		}
	}
}
