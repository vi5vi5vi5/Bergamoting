//go:build !linux

package music

import "errors"

// NewGstDecoder есть только на Linux (колонка): на других системах музыка не играется, тесты подставляют свой декодер.
func NewGstDecoder() (Decoder, error) {
	return nil, errors.New("декодер GStreamer доступен только на колонке (Linux)")
}
