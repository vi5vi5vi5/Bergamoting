//go:build !linux

// Package clock — монотонные часы. Вне Linux это условная замена, нужная лишь чтобы пакеты собирались.
package clock

import "time"

var start = time.Now()

// Mono возвращает время с запуска процесса в наносекундах.
func Mono() int64 { return int64(time.Since(start)) }
