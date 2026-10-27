// Package hw — доступ к светодиодам Станции Мини 3 (bergamot).
package hw

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RingZones — число RGB-зон подсветки логотипа.
const RingZones = 6

// RingDevice — framebuffer подсветки: 6×1 пикселей, 3 байта на пиксель в порядке BGR.
const RingDevice = "/dev/fb0"

type RGB struct{ R, G, B uint8 }

func (c RGB) Hex() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// Scale возвращает цвет при яркости b (0..255).
func (c RGB) Scale(b uint8) RGB {
	k := uint16(b)
	return RGB{uint8(uint16(c.R) * k / 255), uint8(uint16(c.G) * k / 255), uint8(uint16(c.B) * k / 255)}
}

// ParseHex разбирает цвет вида "#rrggbb".
func ParseHex(s string) (RGB, error) {
	h := strings.TrimPrefix(s, "#")
	if len(h) != 6 {
		return RGB{}, fmt.Errorf("цвет %q: нужен формат #rrggbb", s)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return RGB{}, fmt.Errorf("цвет %q: нужен формат #rrggbb", s)
	}
	return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}, nil
}

type RingFrame [RingZones]RGB

type Ring struct{ f *os.File }

func OpenRing() (*Ring, error) {
	f, err := os.OpenFile(RingDevice, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &Ring{f}, nil
}

func (r *Ring) Close() error { return r.f.Close() }

// Read возвращает кадр, который сейчас горит (его мог нарисовать кто угодно).
func (r *Ring) Read() (RingFrame, error) {
	var buf [RingZones * 3]byte
	if _, err := r.f.ReadAt(buf[:], 0); err != nil {
		return RingFrame{}, err
	}
	var fr RingFrame
	for i := range fr {
		fr[i] = RGB{R: buf[i*3+2], G: buf[i*3+1], B: buf[i*3]}
	}
	return fr, nil
}

func (r *Ring) Write(fr RingFrame) error {
	var buf [RingZones * 3]byte
	for i, c := range fr {
		buf[i*3], buf[i*3+1], buf[i*3+2] = c.B, c.G, c.R
	}
	_, err := r.f.WriteAt(buf[:], 0)
	return err
}
