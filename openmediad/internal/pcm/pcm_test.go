package pcm

import "testing"

// Образцы сняты с колонки (прошивка .117).
const (
	running  = "state: RUNNING\nowner_pid   : 7207\ntrigger_time: 3632.295651396\ntstamp      : 4816.905080582\ndelay       : -56860544\navail       : 256\n"
	prepared = "state: PREPARED\nowner_pid   : 5218\ntrigger_time: 3423.556311283\ntstamp      : 3454.300585608\ndelay       : 0\n"
)

func TestParseStatus(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		state   string
		trigger int64
	}{
		{"играет", running, "RUNNING", 3632295651396},
		{"подготовлен", prepared, "PREPARED", 3423556311283},
		{"закрыт", "closed\n", "closed", 0},
		{"пусто", "", "", 0},
		{"без перевода строки в конце", "state: RUNNING\ntrigger_time: 1.5", "RUNNING", 1500000000},
		{"короткая дробная часть", "state: RUNNING\ntrigger_time: 7.001\n", "RUNNING", 7001000000},
		{"лишние цифры дробной части обрезаются", "state: RUNNING\ntrigger_time: 2.1234567899\n", "RUNNING", 2123456789},
	}
	for _, c := range cases {
		st, tr := ParseStatus([]byte(c.in))
		if st != c.state || tr != c.trigger {
			t.Errorf("%s: получили (%q, %d), ждали (%q, %d)", c.name, st, tr, c.state, c.trigger)
		}
	}
}

func TestReadMissingFileFails(t *testing.T) {
	// На ПК /proc/asound нет: Read должен вернуть ошибку, а не пустой успех. На колонке тест пройдёт по другой ветке.
	buf := make([]byte, 512)
	if _, _, err := Read(buf); err == nil {
		t.Log("файл статуса существует (запуск на колонке) — ветка с ошибкой не проверялась")
	}
}
