package hw

import (
	"fmt"
	"unicode"
)

// font — какие сегменты зажигать для символа. Цифры взяты из quasar.cfg
// (leds.clock.frameBuilder.digitValues), буквы — то, что читается на семи сегментах.
var font = map[rune]string{
	' ': "", '-': "G", '_': "D",
	'0': "ABCDEF", '1': "BC", '2': "ABDEG", '3': "ABCDG", '4': "BCFG",
	'5': "ACDFG", '6': "ACDEFG", '7': "ABC", '8': "ABCDEFG", '9': "ABCDFG",
	'A': "ABCEFG", 'b': "CDEFG", 'C': "ADEF", 'c': "DEG", 'd': "BCDEG",
	'E': "ADEFG", 'F': "AEFG", 'G': "ACDEF", 'H': "BCEFG", 'h': "CEFG",
	'I': "EF", 'J': "BCDE", 'L': "DEF", 'n': "CEG", 'o': "CDEG", 'O': "ABCDEF",
	'P': "ABEFG", 'q': "ABCFG", 'r': "EG", 'S': "ACDFG", 't': "DEFG",
	'U': "BCDEF", 'u': "CDE", 'y': "BCDFG",
}

// RenderText раскладывает текст на сегменты: до 4 символов слева направо,
// ':' зажигает двоеточие, '°' — значок градуса. Пример: "12:34", "-HI-", "23°".
func RenderText(text string) ([ClockSegmentCount]bool, error) {
	var segs [ClockSegmentCount]bool
	on := func(name string) {
		i, _ := SegmentIndex(name)
		segs[i] = true
	}
	pos := 0
	for _, ch := range text {
		switch ch {
		case ':':
			on("DH")
			on("DL")
			continue
		case '°':
			on("DEGREE")
			continue
		}
		parts, ok := font[ch]
		if !ok {
			parts, ok = font[unicode.ToUpper(ch)]
		}
		if !ok {
			parts, ok = font[unicode.ToLower(ch)]
		}
		if !ok {
			return segs, fmt.Errorf("символ %q не нарисовать на семи сегментах", ch)
		}
		if pos >= 4 {
			return segs, fmt.Errorf("в %q больше 4 символов", text)
		}
		for _, p := range parts {
			on(fmt.Sprintf("%c%d", p, pos))
		}
		pos++
	}
	return segs, nil
}
