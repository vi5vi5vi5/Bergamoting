package pb

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var b []byte
	b = AppendUint(b, 1, 300)
	b = AppendString(b, 2, "привет")
	b = AppendBytes(b, 3, []byte{1, 2, 3})
	b = append(b, 4<<3|WireFixed32, 0x78, 0x56, 0x34, 0x12)
	b = append(b, 5<<3|WireFixed64, 1, 0, 0, 0, 0, 0, 0, 0x80)

	r := NewReader(b)
	want := []Field{
		{Num: 1, Wire: WireVarint, U: 300},
		{Num: 2, Wire: WireBytes, B: []byte("привет")},
		{Num: 3, Wire: WireBytes, B: []byte{1, 2, 3}},
		{Num: 4, Wire: WireFixed32, U: 0x12345678},
		{Num: 5, Wire: WireFixed64, U: 0x8000000000000001},
	}
	for i, w := range want {
		f, err := r.Next()
		if err != nil {
			t.Fatalf("поле %d: %v", i, err)
		}
		if f.Num != w.Num || f.Wire != w.Wire || f.U != w.U || !bytes.Equal(f.B, w.B) {
			t.Fatalf("поле %d: получили %+v, ждали %+v", i, f, w)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("после последнего поля ждали io.EOF, получили %v", err)
	}
	if err := Validate(b); err != nil {
		t.Fatal(err)
	}
}

func TestVarintBoundaries(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, 1<<32 - 1, 1 << 32, 1<<63 - 1, 1 << 63, 1<<64 - 1} {
		b := AppendUint(nil, 1, v)
		got, ok := Uint(b, 1)
		if !ok || got != v {
			t.Errorf("%d: получили %d, ok=%v", v, got, ok)
		}
	}
}

func TestMalformed(t *testing.T) {
	cases := map[string]struct {
		in  []byte
		err error
	}{
		"обрезанные bytes":    {[]byte{0x0a, 0x05, 'a', 'b'}, ErrTruncated},
		"длина больше буфера": {[]byte{0x0a, 0xff, 0xff, 0xff, 0x7f}, ErrTruncated},
		"поле 0":              {[]byte{0x00, 0x01}, ErrFieldZero},
		"тип провода 3":       {[]byte{0x0b}, ErrWireType},
		"тип провода 4":       {[]byte{0x0c}, ErrWireType},
		"обрезанный varint":   {[]byte{0x08, 0x80}, ErrTruncated},
		"переполнение":        {[]byte{0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, ErrVarint},
		"обрезанный fixed32":  {[]byte{4<<3 | WireFixed32, 1, 2}, ErrTruncated},
		"обрезанный fixed64":  {[]byte{5<<3 | WireFixed64, 1, 2, 3}, ErrTruncated},
	}
	for name, c := range cases {
		if err := Validate(c.in); !errors.Is(err, c.err) {
			t.Errorf("%s: получили %v, ждали %v", name, err, c.err)
		}
	}
}

func TestFindOnBrokenInput(t *testing.T) {
	good := AppendUint(nil, 1, 7)
	broken := append(good, 0x0a, 0x09, 'x') // второе поле обрезано
	if v, ok := Uint(broken, 1); !ok || v != 7 {
		t.Errorf("поле до обрыва должно читаться: %d %v", v, ok)
	}
	if _, ok := Uint(broken, 2); ok {
		t.Error("обрезанное поле не должно находиться")
	}
}

func TestHelpers(t *testing.T) {
	inner := AppendString(nil, 6, "activation")
	var b []byte
	b = AppendBytes(b, 10, inner)
	b = AppendString(b, 2, "текст")
	b = AppendUint(b, 1, 3)

	if !Has(b, 10) || Has(b, 99) {
		t.Error("Has")
	}
	if s, _ := Str(b, 2); s != "текст" {
		t.Errorf("Str: %q", s)
	}
	if _, ok := Uint(b, 2); ok {
		t.Error("Uint не должен читать bytes-поле")
	}
	if _, ok := Bytes(b, 1); ok {
		t.Error("Bytes не должен читать varint-поле")
	}
	in, _ := Bytes(b, 10)
	if s, _ := Str(in, 6); s != "activation" {
		t.Errorf("вложенное поле: %q", s)
	}
	nums := Nums(append(b, AppendUint(nil, 1, 9)...))
	if len(nums) != 3 || nums[0] != 10 || nums[1] != 2 || nums[2] != 1 {
		t.Errorf("Nums без повторов по порядку: %v", nums)
	}
}

func TestReaderDoesNotCopy(t *testing.T) {
	b := AppendBytes(nil, 1, []byte("abc"))
	f, _ := NewReader(b).Next()
	b[len(b)-1] = 'X'
	if string(f.B) != "abX" {
		t.Errorf("Field.B должен ссылаться на исходный буфер, получили %q", f.B)
	}
}

func TestDump(t *testing.T) {
	inner := AppendString(AppendString(nil, 1, "644732"), 2, "https://x/y")
	msg := AppendBytes(nil, 1, inner)
	msg = AppendUint(msg, 3, 42)
	msg = AppendString(msg, 4, "музыка")
	msg = AppendBytes(msg, 5, []byte{0xff, 0xfe, 0x00, 0x01})
	got := Dump(msg, 100)
	want := `1:{1:"644732" 2:"https://x/y"} 3:42 4:"музыка" 5:xfffe0001`
	if got != want {
		t.Fatalf("Dump:\n got %s\nwant %s", got, want)
	}
	if got := Dump(AppendString(nil, 1, strings.Repeat("a", 50)), 10); got != `1:"aaaaaaaaaa"…(50)` {
		t.Fatalf("обрезка: %s", got)
	}
}

func TestAppendFieldRoundTrip(t *testing.T) {
	var b []byte
	b = AppendUint(b, 1, 77)
	b = AppendFixed64(b, 2, 0x3ff0000000000000)
	b = AppendBytes(b, 3, []byte("xyz"))
	b = append(b, 4<<3|WireFixed32, 0x78, 0x56, 0x34, 0x12)
	var out []byte
	r := NewReader(b)
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		out = AppendField(out, f)
	}
	if !bytes.Equal(out, b) {
		t.Fatalf("пересборка полей изменила сообщение:\n got %x\nwant %x", out, b)
	}
}
