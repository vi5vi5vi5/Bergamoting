//go:build !linux

package sound

import "errors"

// DecodeFile есть только на Linux (колонка): на других системах звонки не играются, тесты подставляют Engine.Decode.
func DecodeFile(path string) (*Clip, error) {
	return nil, errors.New("декодер GStreamer доступен только на колонке (Linux)")
}
