package hw

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ClockSegmentCount — сколько всего сегментов у часов.
const ClockSegmentCount = 4*7 + 4

// ClockSegments — имена сегментов в фиксированном порядке: цифры 0..3 слева
// направо по сегментам A..G (A — верх, далее по часовой, G — середина), затем
// двоеточие (DH — верхняя точка, DL — нижняя), средняя точка DM и значок градуса.
var ClockSegments = func() []string {
	s := make([]string, 0, ClockSegmentCount)
	for pos := 0; pos < 4; pos++ {
		for _, part := range "ABCDEFG" {
			s = append(s, fmt.Sprintf("%c%d", part, pos))
		}
	}
	return append(s, "DH", "DL", "DM", "DEGREE")
}()

var segmentIndex = func() map[string]int {
	m := make(map[string]int, ClockSegmentCount)
	for i, name := range ClockSegments {
		m[name] = i
	}
	return m
}()

// SegmentIndex возвращает номер сегмента в ClockFrame по его имени ("A0", "DH", ...).
func SegmentIndex(name string) (int, bool) {
	i, ok := segmentIndex[name]
	return i, ok
}

const clockPath = "/sys/class/leds/bergamot:white:clock%s/brightness"

// ClockFrame — яркость каждого сегмента (0..255) в порядке ClockSegments.
type ClockFrame [ClockSegmentCount]uint8

// Clock держит открытыми файлы яркости всех сегментов: sysfs позволяет
// перечитывать и перезаписывать их с нулевого смещения без переоткрытия.
type Clock struct{ files []*os.File }

func OpenClock() (*Clock, error) {
	c := &Clock{}
	for _, name := range ClockSegments {
		f, err := os.OpenFile(fmt.Sprintf(clockPath, name), os.O_RDWR, 0)
		if err != nil {
			c.Close()
			return nil, err
		}
		c.files = append(c.files, f)
	}
	return c, nil
}

func (c *Clock) Close() error {
	var errs []error
	for _, f := range c.files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

func (c *Clock) Read() (ClockFrame, error) {
	var fr ClockFrame
	var buf [8]byte
	for i, f := range c.files {
		n, err := f.ReadAt(buf[:], 0)
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return fr, fmt.Errorf("%s: %w", ClockSegments[i], err)
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
		if err != nil {
			return fr, fmt.Errorf("%s: %w", ClockSegments[i], err)
		}
		fr[i] = uint8(v)
	}
	return fr, nil
}

// Write выставляет яркость только тем сегментам, у которых она отличается от prev.
func (c *Clock) Write(fr, prev ClockFrame) error {
	for i, f := range c.files {
		if fr[i] == prev[i] {
			continue
		}
		if _, err := f.WriteAt([]byte(strconv.Itoa(int(fr[i]))), 0); err != nil {
			return fmt.Errorf("%s: %w", ClockSegments[i], err)
		}
	}
	return nil
}
