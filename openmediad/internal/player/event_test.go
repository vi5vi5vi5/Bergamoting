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

// capturesDir — каталог с записями живого трафика (в .gitignore: там идентификаторы аккаунта).
func capturesDir(t testing.TB) string {
	dir := os.Getenv("OMD_CAPTURES")
	if dir == "" {
		dir = filepath.Join("..", "..", "captures")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.pcap")); len(m) == 0 {
		t.Skipf("нет записей в %s (OMD_CAPTURES)", dir)
	}
	return dir
}

// snapFrom вынимает из настоящего события входные данные синтезатора.
func snapFrom(ev []byte, cmdDesc []byte) Snap {
	var s Snap
	s.Kind = quasar.AudioEvent(ev).Kind()
	s.State = quasar.AudioEvent(ev).State()
	d, _ := pb.Bytes(ev, 3)
	s.PosMs, _ = pb.Uint(d, 28)
	s.Idx, _ = pb.Uint(d, 12)
	s.DurS, _ = pb.Uint(d, 6)
	s.HasDur = pb.Has(d, 6)
	s.Mono, _ = pb.Uint(d, 29)
	s.UUID, _ = pb.Str(d, 22)
	url, _ := pb.Str(d, 2)
	s.Vsid = VsidOf(url)
	s.Start, _ = pb.Uint(ev, 5)
	s.Stop, _ = pb.Uint(ev, 7)
	if v, ok := pb.Uint(ev, 9); ok {
		s.HasT9, s.T9 = true, v == 1
	}
	if v, ok := pb.Uint(ev, 17); ok {
		s.HasT17, s.T17 = true, v == 1
	}
	s.Gain, _ = pb.Uint(ev, 12)
	// Поля дескриптора, где событие расходится с командой не по общим правилам, идут в Over.
	for _, n := range []uint64{2, 42, 35, 46} {
		got := descFields(d, n, true)
		want := descFields(cmdDesc, n, false)
		if !equalFields(got, want) {
			s.Over = append(s.Over, got...)
		}
	}
	r := pb.NewReader(ev)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		if f.Num == 14 {
			s.Diag = pb.AppendField(s.Diag, f)
		}
	}
	return s
}

// descFields — все поля номер n дескриптора; для 2 и 42 при strip убирается суффикс «?vsid=…».
func descFields(d []byte, n uint64, strip bool) (out []pb.Field) {
	r := pb.NewReader(d)
	for {
		f, err := r.Next()
		if err != nil {
			return
		}
		if f.Num != n {
			continue
		}
		if strip && (n == 2 || n == 42) {
			if i := bytes.Index(f.B, []byte("?vsid=")); i >= 0 {
				f.B = f.B[:i]
			}
		}
		out = append(out, f)
	}
}

func equalFields(a, b []pb.Field) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Num != b[i].Num || a[i].Wire != b[i].Wire || a[i].U != b[i].U || !bytes.Equal(a[i].B, b[i].B) {
			return false
		}
	}
	return true
}

// isSynthetic узнаёт событие, собранное нашим шимом, а не настоящим mediad: в записях со стороны шима такие события видны
// на клиентской стороне (после REDIRECT они идут «с порта 9906») и перемешаны с настоящими. У наших событий в diag всегда
// rss_mem = 33000 (константа), у настоящих это измеренное число.
func isSynthetic(ev []byte) bool {
	var rss, bitrate uint64
	r := pb.NewReader(ev)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		if f.Num != 14 {
			continue
		}
		name, _ := pb.Str(f.B, 1)
		v, _ := pb.Uint(f.B, 3)
		switch name {
		case "rss_mem":
			rss = v
		case "bitrate":
			bitrate = v
		}
	}
	_ = bitrate
	return rss == 33000
}

// firstDiff называет первое расхождение двух сообщений: «поле N» или «поле N.M».
func firstDiff(a, b []byte, path string) string {
	ra, rb := pb.NewReader(a), pb.NewReader(b)
	for {
		fa, ea := ra.Next()
		fb, eb := rb.Next()
		if ea != nil || eb != nil {
			if ea != eb {
				return path + " (разная длина)"
			}
			return ""
		}
		p := fmt.Sprintf("%s%d", path, fa.Num)
		if fa.Num != fb.Num {
			return fmt.Sprintf("%s≠%d (порядок/состав)", p, fb.Num)
		}
		if fa.Wire == pb.WireBytes && fb.Wire == pb.WireBytes && !bytes.Equal(fa.B, fb.B) {
			if (fa.Num == 3 || fa.Num == 10 || fa.Num == 21) && pb.Validate(fa.B) == nil {
				return firstDiff(fa.B, fb.B, p+".")
			}
			return p
		}
		if fa.Wire != fb.Wire || fa.U != fb.U {
			return p
		}
	}
}

// TestEventsMatchRecordings: из команды play и данных события (позиция, uuid, vsid, метки времени, diag) синтезатор
// обязан получить событие байт в байт. Так проверяется всё, что не является «входом»: порядок и состав полей,
// правила переписывания URL, поля 21/26/27/30 и т. п.
func TestEventsMatchRecordings(t *testing.T) {
	dir := capturesDir(t)
	files, _ := filepath.Glob(filepath.Join(dir, "*.pcap"))
	total, bad, over := 0, map[string]int{}, map[string]int{}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		plays := map[string]quasar.AudioCommand{}
		prefs := map[string]bool{}
		paused := map[string]bool{} // трек, чей плеер стоит на паузе (mediad держит его в кэше)
		reused := map[string]bool{}
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
				switch c.Classify() {
				case quasar.CmdPlay:
					id := c.Player().ID
					plays[id] = c
					reused[id] = prefs[id] || paused[c.Player().Stream]
				case quasar.CmdPrefetch:
					prefs[c.Player().ID] = true
				}
			case wire.TypeAudioEvent:
				e := quasar.AudioEvent(m.Body)
				p := e.Player()
				if p.Kind != quasar.PlayerAudio || e.Kind() == 5 || isSynthetic(m.Body) {
					return
				}
				play, ok := plays[p.ID]
				if !ok {
					// События плееров, чьих команд в записи нет (их съел шим, это его собственные события), не сверяются и
					// не должны влиять на учёт паузы: иначе наш PAUSED при передаче mediad сделал бы холодный запуск «тёплым».
					return
				}
				if e.Kind() == 1 {
					switch e.State() {
					case quasar.PlayState(4):
						paused[p.Stream] = true
					case quasar.PlayState(3):
						delete(paused, p.Stream)
					}
				}
				meta := MetaFrom(play, reused[p.ID])
				sn := snapFrom(m.Body, meta.Desc)
				got := meta.Event(sn)
				for _, f := range sn.Over {
					over[fmt.Sprint(f.Num)]++
				}
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
		t.Fatal("в записях нет событий музыкального плеера — проверка вхолостую")
	}
	t.Logf("событий сверено: %d, не совпало: %d; потребовалась подстановка полей дескриптора: %v", total, sum(bad), over)
	var keys []string
	for k := range bad {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Errorf("расхождение %-40s × %d", k, bad[k])
	}
}

func sum(m map[string]int) (n int) {
	for _, v := range m {
		n += v
	}
	return
}
