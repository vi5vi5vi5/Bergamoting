package pb

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Dump печатает сообщение деревом в одну строку: «1:{1:"id" 2:"url" 3:0} 22:{…}». Схем нет, поэтому тип поля
// угадывается: печатный UTF-8 — строка, иначе то, что разбирается как protobuf, — вложенное сообщение, иначе hex.
// Длинные значения обрезаются до max байт; в конце ставится «…(полный размер)».
func Dump(b []byte, max int) string {
	var sb strings.Builder
	dump(&sb, b, max, 0)
	return sb.String()
}

func dump(sb *strings.Builder, b []byte, max, depth int) {
	r := NewReader(b)
	first := true
	for {
		f, err := r.Next()
		if err != nil {
			if err != io.EOF {
				sb.WriteString(" ?!" + err.Error())
			}
			return
		}
		if !first {
			sb.WriteByte(' ')
		}
		first = false
		fmt.Fprintf(sb, "%d:", f.Num)
		switch f.Wire {
		case WireVarint:
			fmt.Fprintf(sb, "%d", f.U)
		case WireFixed32:
			fmt.Fprintf(sb, "f32(%#x)", f.U)
		case WireFixed64:
			fmt.Fprintf(sb, "f64(%#x)", f.U)
		case WireBytes:
			dumpBytes(sb, f.B, max, depth)
		}
	}
}

func dumpBytes(sb *strings.Builder, v []byte, max, depth int) {
	switch {
	case len(v) == 0:
		sb.WriteString(`""`)
	case printable(v):
		fmt.Fprintf(sb, "%q", clip(v, max))
		if len(v) > max {
			fmt.Fprintf(sb, "…(%d)", len(v))
		}
	case depth < 8 && Validate(v) == nil:
		sb.WriteByte('{')
		dump(sb, v, max, depth+1)
		sb.WriteByte('}')
	default:
		n := min(len(v), max/4+4)
		fmt.Fprintf(sb, "x%x", v[:n])
		if len(v) > n {
			fmt.Fprintf(sb, "…(%d)", len(v))
		}
	}
}

func clip(v []byte, max int) string {
	if len(v) <= max {
		return string(v)
	}
	s := string(v[:max])
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func printable(v []byte) bool {
	if !utf8.Valid(v) {
		return false
	}
	for _, r := range string(v) {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}
