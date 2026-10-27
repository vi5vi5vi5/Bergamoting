package hw

import "testing"

func lit(segs [ClockSegmentCount]bool) map[string]bool {
	m := map[string]bool{}
	for i, on := range segs {
		if on {
			m[ClockSegments[i]] = true
		}
	}
	return m
}

func TestRenderText(t *testing.T) {
	// Так прошивка показывала 22:20 — снято с колонки.
	segs, err := RenderText("22:20")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A0", "B0", "D0", "E0", "G0", "A1", "B1", "D1", "E1", "G1",
		"A2", "B2", "D2", "E2", "G2", "A3", "B3", "C3", "D3", "E3", "F3", "DH", "DL"}
	got := lit(segs)
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, s := range want {
		if !got[s] {
			t.Errorf("%s не горит", s)
		}
	}

	if _, err := RenderText("12345"); err == nil {
		t.Error("5 символов должны дать ошибку")
	}
	if _, err := RenderText("ж"); err == nil {
		t.Error("неизвестный символ должен дать ошибку")
	}
	if segs, err := RenderText("hi"); err != nil || !lit(segs)["E0"] {
		t.Errorf("hi: %v %v", lit(segs), err)
	}
}
