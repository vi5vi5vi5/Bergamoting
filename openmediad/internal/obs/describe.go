// Package obs — наблюдение за шиной: человекочитаемое описание кадров, клиент-наблюдатель, разбор pcap.
package obs

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"openmediad/internal/pb"
	"openmediad/internal/quasar"
	"openmediad/internal/wire"
)

// Opt — что показывать. По умолчанию скрыт фон: прогресс плеера (1 Гц), iohub (спектр 20 Гц), метрики.
type Opt struct {
	Progress bool // события прогресса и диагностики плеера
	Noisy    bool // iohub, metricad и прочие фоновые сообщения

	// VoiceDir — только для разбора pcap: класть в этот каталог голосовые чанки Алисы (Ogg/Opus) файлами .ogg.
	VoiceDir string

	// Dump — только для разбора pcap: после строки команды/события плеера печатать дерево полей целиком
	// (длинные значения обрезаются до DumpMax байт, по умолчанию 160).
	Dump    bool
	DumpMax int
	// DumpTypes — какие типы сообщений печатать деревом; пусто — только команды и события плеера (8 и 127).
	DumpTypes map[wire.Type]bool
}

// Describe возвращает строку для кадра. show=false — кадр нужно только посчитать, но не печатать.
func Describe(svc string, m wire.Msg, o Opt) (line string, show bool) {
	switch m.Type {
	case wire.TypeAliceState:
		return describeAlice(quasar.AliceState(m.Body)), true

	case wire.TypeAudioEvent:
		e := quasar.AudioEvent(m.Body)
		if k := e.Kind(); (k == 2 || k == 5) && !o.Progress {
			return "", false
		}
		p := e.Player()
		var b strings.Builder
		fmt.Fprintf(&b, "AudioEvent %s %s %s/%s", e.Kind(), e.State(), p.Kind, short(p.ID))
		if t := e.TrackID(); t != "" {
			b.WriteString(" track=" + t)
		}
		if pos, ok := e.PositionMs(); ok {
			fmt.Fprintf(&b, " pos=%.1fs", float64(pos)/1000)
		}
		return b.String(), true

	case wire.TypeAudioCommand:
		return describeCommand(quasar.AudioCommand(m.Body)), true

	case wire.TypeRemoting:
		r := quasar.Remoting(m.Body)
		s := fmt.Sprintf("Remoting %s methods=%v", r.Object(), r.Methods())
		if ev, ok := pb.Bytes(r, 18); ok {
			k, _ := pb.Uint(ev, 1)
			s += fmt.Sprintf(" event=%d", k)
		}
		return s, true

	case wire.TypeHandshake:
		h := quasar.Handshake(m.Body)
		app, ver, _, model := h.Device()
		return fmt.Sprintf("Handshake app=%s ver=%s model=%s features=%d", app, ver, model, len(h.Features())), true
	}
	if !o.Noisy {
		return "", false
	}
	return fmt.Sprintf("%s len=%d", m.Type, len(m.Raw)), true
}

func describeAlice(s quasar.AliceState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "AliceState %s", s.Kind())
	if t := s.Text(); t != "" {
		fmt.Fprintf(&b, " text=%q", t)
	}
	if _, final, ok := s.ASR(); ok && final {
		b.WriteString(" final")
	}
	if id := s.RequestID(); id != "" {
		b.WriteString(" req=" + short(id))
	}
	if speech, js, ok := s.Reply(); ok {
		fmt.Fprintf(&b, " reply=%q directives=%v", speech, directiveNames(js))
	}
	return b.String()
}

func describeCommand(c quasar.AudioCommand) string {
	p := c.Player()
	kind := c.Classify()
	var b strings.Builder
	fmt.Fprintf(&b, "AudioCommand %s %s/%s", kind, p.Kind, short(p.ID))
	switch kind {
	case quasar.CmdVoiceChunk:
		chunk, _ := c.VoiceChunk()
		fmt.Fprintf(&b, " %dB", len(chunk))
	case quasar.CmdPlay, quasar.CmdPrefetch:
		if t, ok := c.Track(); ok {
			fmt.Fprintf(&b, " track=%s off=%dms", t.ID, t.OffsetMs)
			if t.Title != "" {
				fmt.Fprintf(&b, " %q", t.Artist+" — "+t.Title)
			}
			b.WriteString(" src=" + urlHost(t.URL))
		}
	}
	if r := c.Reason(); r != "" {
		fmt.Fprintf(&b, " reason=%q", r)
	}
	return b.String()
}

// directiveNames достаёт имена директив из JSON ответа облака (response.directives[].name).
func directiveNames(js []byte) []string {
	var v struct {
		Response struct {
			Directives []struct {
				Name string `json:"name"`
			} `json:"directives"`
		} `json:"response"`
	}
	if json.Unmarshal(js, &v) != nil {
		return nil
	}
	out := make([]string, 0, len(v.Response.Directives))
	for _, d := range v.Response.Directives {
		out = append(out, d.Name)
	}
	return out
}

// urlHost оставляет только хост: в ссылках на потоки лежат подписанные токены, их не печатаем.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
