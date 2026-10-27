// Package iohub — сообщения спектра для сервиса iohub (127.0.0.1:9903) и клиент, который их отправляет.
//
// Штатный mediad подключён к iohub как обычный клиент и 25 раз в секунду шлёт сообщение IOHub (тип 85) с полем 46:
// спектр звука для визуализации на дисплее и светодиодах. Первым при подключении клиент ничего не отправляет; сервис
// сам присылает ему служебные сообщения, которые читать не нужно, но нужно вычитывать. Формат восстановлен по записям
// (iohub_services C→S, поле 46) и проверен побайтной пересборкой настоящих кадров (iohub_test.go).
package iohub

import (
	"openmediad/internal/pb"
	"openmediad/internal/wire"
)

// Вид сообщения спектра (поле 46.6).
const (
	KindData   = 0 // кадр спектра
	KindStart  = 1 // поток начался (один раз, до первого кадра)
	KindStop   = 2 // поток остановлен: пауза, конец, вытеснение (mediad шлёт 1–2 раза)
	KindResume = 3 // поток продолжен после паузы (тот же идентификатор потока)
)

// Источник звука (поле 46.8).
const (
	SourceMusic = 0
	SourceVoice = 1 // голос Алисы: его спектр по-прежнему шлёт mediad
)

// Параметры спектра, как у штатного элемента `spectrum interval=40000000 bands=513 threshold=-80`.
const (
	intervalNs = 40_000_000
	sampleRate = 48000
)

// thresholdField — порог −80 дБ в поле 46.5: в данных 18446744073709551536, то есть int64(−80), записанный как uint64.
var thresholdField = func() uint64 { v := int64(-80); return uint64(v) }()

// Data собирает кадр спектра. tsNs — время кадра, Unix-наносекунды; db — 513 значений в дБ; stream — идентификатор потока
// (у mediad CLOCK_MONOTONIC в наносекундах на момент старта потока).
func Data(tsNs uint64, db []float32, stream, source uint64) []byte {
	raw := make([]byte, 4*len(db))
	for i, v := range db {
		putF32(raw[4*i:], v)
	}
	var in []byte
	in = pb.AppendUint(in, 1, tsNs)
	in = pb.AppendUint(in, 2, intervalNs)
	in = pb.AppendUint(in, 3, sampleRate)
	in = pb.AppendBytes(in, 4, raw)
	in = pb.AppendUint(in, 5, thresholdField) // −80 как int64: отрицательное число занимает десять байт varint
	in = pb.AppendUint(in, 6, KindData)
	in = pb.AppendUint(in, 7, stream)
	in = pb.AppendUint(in, 8, source)
	return wrap(in)
}

// Control собирает служебное сообщение потока (старт, остановка, продолжение).
func Control(kind, stream, source uint64) []byte {
	var in []byte
	in = pb.AppendUint(in, 6, kind)
	in = pb.AppendUint(in, 7, stream)
	in = pb.AppendUint(in, 8, source)
	return wrap(in)
}

func wrap(spectrum []byte) []byte {
	return wire.Wrap(wire.TypeIOHub, pb.AppendBytes(nil, 46, spectrum))
}

func putF32(b []byte, v float32) {
	u := f32bits(v)
	b[0], b[1], b[2], b[3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
}
