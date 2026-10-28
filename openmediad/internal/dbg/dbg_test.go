package dbg

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
	"openmediad/internal/wire"
)

func TestRingKeepsNewestInOrder(t *testing.T) {
	h := &Hub{Cap: 5}
	for i := 1; i <= 12; i++ {
		h.Log(Info, "t", "m", "", map[string]any{"i": i})
	}
	es := h.Since(0, 0)
	if len(es) != 5 {
		t.Fatalf("в кольце %d записей, ждали 5", len(es))
	}
	for i, e := range es {
		if want := uint64(8 + i); e.ID != want {
			t.Fatalf("запись %d имеет ID %d, ждали %d: порядок или состав нарушен", i, e.ID, want)
		}
	}
	if got := h.Since(10, 0); len(got) != 2 || got[0].ID != 11 {
		t.Fatalf("Since(10) вернул %v", got)
	}
	if got := h.Since(0, 2); len(got) != 2 || got[1].ID != 12 {
		t.Fatalf("limit должен оставлять самые свежие: %v", got)
	}
}

func TestSlowSubscriberDoesNotBlockLogging(t *testing.T) {
	h := &Hub{}
	ch, unsub := h.Subscribe()
	defer unsub()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ { // подписчик не читает, канал на 256
			h.Log(Info, "t", "m", "", nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("журнал завис на медленном подписчике")
	}
	if len(ch) != 256 {
		t.Fatalf("в канале %d записей, ждали 256 (остальные пропущены)", len(ch))
	}
}

// realLikeCommand — play с ключом и подписанным URL, как в трафике.
func realLikeCommand() quasar.AudioCommand {
	md := pb.AppendString(nil, 1, "Hot N Cold")
	md = pb.AppendString(md, 2, "Katy Perry")
	d := pb.AppendString(nil, 1, "322454")
	d = pb.AppendString(d, 2, "https://strm-m9-2.strm.yandex.net/music-v2/crypt/ysign1=SECRETSIGNATURE,kts=1/0/1/aac192-mp4")
	d = pb.AppendString(d, 7, `{"callbacks":{"payload":{"@request_id":"SECRET-REQUEST-ID"}}}`)
	d = pb.AppendBytes(d, 9, md)
	d = pb.AppendString(d, 39, "8a9186c2262401e8226d78b000ff58b1")
	pl := pb.AppendUint(nil, 1, 1)
	pl = pb.AppendString(pl, 2, "2be3a8cf-df6a-4da5-b816-e89a568f978d")
	c := pb.AppendBytes(nil, 1, d)
	c = pb.AppendBytes(c, 22, pl)
	return pb.AppendString(c, 52, "AUDIO_PLAY directive")
}

func TestNoSecretsInEntries(t *testing.T) {
	frame := wire.Wrap(wire.TypeAudioCommand, realLikeCommand())
	e, ok := DescribeFrame(true, frame)
	if !ok {
		t.Fatal("кадр должен описываться")
	}
	b, _ := json.Marshal(e)
	for _, secret := range []string{"8a9186c2262401e8226d78b000ff58b1", "SECRETSIGNATURE", "SECRET-REQUEST-ID", "ysign1"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("в записи журнала просочилось %q: %s", secret, b)
		}
	}
	for _, want := range []string{"Hot N Cold", "Katy Perry", "strm-m9-2.strm.yandex.net", "322454"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("в записи нет %q: %s", want, b)
		}
	}
	if e.Why == "" {
		t.Fatal("у записи должно быть пояснение простыми словами")
	}
}

func TestExplainCoversVoiceAndEvents(t *testing.T) {
	voice := pb.AppendBytes(nil, 22, pb.AppendUint(pb.AppendString(nil, 2, "v1"), 1, 2))
	voice = pb.AppendString(voice, 29, "OggS....")
	e, _ := DescribeFrame(true, wire.Wrap(wire.TypeAudioCommand, voice))
	if !strings.Contains(e.Msg, "голос Алисы") || e.Lvl != Trace {
		t.Fatalf("голосовой фрагмент: %q уровень %s", e.Msg, e.Lvl)
	}
	focus := pb.AppendUint(nil, 39, 1)
	e, _ = DescribeFrame(true, wire.Wrap(wire.TypeAudioCommand, focus))
	if !strings.Contains(e.Msg, "диалог занял") {
		t.Fatalf("focus 1: %q", e.Msg)
	}
	ev := pb.AppendUint(nil, 1, 1)
	ev = pb.AppendUint(ev, 2, 5)
	ev = pb.AppendBytes(ev, 4, pb.AppendString(pb.AppendUint(nil, 1, 1), 2, "abcdef0123456789"))
	e, _ = DescribeFrame(false, wire.Wrap(wire.TypeAudioEvent, ev))
	if !strings.Contains(e.Msg, "закончил") || e.Lvl != Info {
		t.Fatalf("ENDED: %q уровень %s", e.Msg, e.Lvl)
	}
	prog := pb.AppendUint(nil, 1, 2)
	prog = pb.AppendUint(prog, 2, 3)
	e, _ = DescribeFrame(false, wire.Wrap(wire.TypeAudioEvent, prog))
	if e.Lvl != Trace {
		t.Fatal("progress — фон")
	}
	if e, _ := DescribeFrame(true, []byte{1, 2, 3}); e.Lvl != Warn {
		t.Fatal("кадр без конверта — предупреждение")
	}
}

func newTestServer(t *testing.T, cors bool) (*Hub, *httptest.Server) {
	h := &Hub{}
	s := &Server{Hub: h, CORS: cors, State: func() any { return map[string]any{"mode": "тест"} }}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return h, ts
}

func TestHTTPEndpoints(t *testing.T) {
	h, ts := newTestServer(t, false)
	h.Log(Info, "shim", "первая", "пояснение", map[string]any{"k": "v"})
	h.Log(Warn, "music", "вторая", "", nil)

	resp, err := http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("страница: %v %v", resp, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/api/stream") {
		t.Fatal("страница должна подключаться к /api/stream")
	}
	if r, _ := http.Get(ts.URL + "/нет"); r.StatusCode != 404 {
		t.Fatalf("лишний путь: %d", r.StatusCode)
	}

	var es []Entry
	resp, _ = http.Get(ts.URL + "/api/log?since=1")
	json.NewDecoder(resp.Body).Decode(&es)
	if len(es) != 1 || es[0].Msg != "вторая" {
		t.Fatalf("/api/log?since=1: %+v", es)
	}
	resp, _ = http.Get(ts.URL + "/api/log?format=ndjson")
	nd, _ := io.ReadAll(resp.Body)
	if n := strings.Count(strings.TrimSpace(string(nd)), "\n") + 1; n != 2 {
		t.Fatalf("ndjson: %d строк, ждали 2", n)
	}

	var st map[string]any
	resp, _ = http.Get(ts.URL + "/api/state")
	json.NewDecoder(resp.Body).Decode(&st)
	if st["shim"].(map[string]any)["mode"] != "тест" || st["uptime_s"] == nil {
		t.Fatalf("/api/state: %v", st)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS по умолчанию выключен")
	}
	if r, _ := http.Post(ts.URL+"/api/log", "text/plain", nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("запись через порт невозможна, а POST вернул %d", r.StatusCode)
	}
}

func TestCORSOnlyWhenEnabled(t *testing.T) {
	_, ts := newTestServer(t, true)
	resp, _ := http.Get(ts.URL + "/api/state")
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("с CORS=true заголовок должен быть")
	}
}

func TestStreamSendsBacklogThenLive(t *testing.T) {
	h, ts := newTestServer(t, false)
	h.Log(Info, "shim", "старая", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	read := func() Entry {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "data: ") {
				var e Entry
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
				return e
			}
		}
	}
	if e := read(); e.Msg != "старая" {
		t.Fatalf("сначала накопленное, а пришло %q", e.Msg)
	}
	go func() { time.Sleep(100 * time.Millisecond); h.Log(Info, "shim", "новая", "", nil) }()
	if e := read(); e.Msg != "новая" {
		t.Fatalf("затем новое, а пришло %q", e.Msg)
	}
}

func TestStreamLimit(t *testing.T) {
	_, ts := newTestServer(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var bodies []io.Closer
	for i := 0; i < maxStreams; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("поток %d: %v", i, err)
		}
		bodies = append(bodies, resp.Body)
	}
	time.Sleep(100 * time.Millisecond)
	resp, err := http.Get(ts.URL + "/api/stream")
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("лишний слушатель должен получить 503: %v %v", resp, err)
	}
	for _, b := range bodies {
		b.Close()
	}
}

func TestExplainFileStartTellsRingFromOneShot(t *testing.T) {
	for _, tc := range []struct {
		loop uint64
		want string
	}{{0, "звонок"}, {1, "системный звук"}} {
		d := pb.AppendString(pb.AppendUint(nil, 4, 4), 25, "/system/vendor/quasar/sounds/common/x.mp3")
		d = pb.AppendUint(d, 50, tc.loop)
		c := pb.AppendBytes(nil, 1, d)
		c = pb.AppendBytes(c, 22, pb.AppendString(pb.AppendUint(nil, 1, 4), 2, "f1e2d3c4-0000"))
		e, ok := DescribeFrame(true, wire.Wrap(wire.TypeAudioCommand, c))
		if !ok || !strings.Contains(e.Msg, tc.want) || e.Data["file_loops"] != (tc.loop == 0) {
			t.Errorf("50:%d: msg=%q data=%v", tc.loop, e.Msg, e.Data)
		}
	}
}

func TestExplainFileStartShowsSystemSoundPath(t *testing.T) {
	d := pb.AppendUint(nil, 4, 4)
	d = pb.AppendString(d, 25, "/system/vendor/quasar/sounds/common/timer.wav")
	c := pb.AppendBytes(nil, 1, d)
	c = pb.AppendBytes(c, 22, pb.AppendString(pb.AppendUint(nil, 1, 4), 2, "f1e2d3c4-0000"))
	e, ok := DescribeFrame(true, wire.Wrap(wire.TypeAudioCommand, c))
	if !ok {
		t.Fatal("кадр должен описываться")
	}
	if !strings.Contains(e.Msg, "timer.wav") || e.Why == "" || e.Data["file"] != "/system/vendor/quasar/sounds/common/timer.wav" {
		t.Fatalf("запуск файла: msg=%q why=%q data=%v", e.Msg, e.Why, e.Data)
	}
}
