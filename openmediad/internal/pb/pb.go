// Package pb — минимальный разбор и сборка protobuf wire-формата без схем.
//
// Схем шины Quasar в прошивке нет (maind собран в lite-режиме), поэтому поля читаются по номерам.
// Разбор не выделяет память: Field.B ссылается на исходный буфер.
package pb

import (
	"errors"
	"io"
)

// Типы провода protobuf.
const (
	WireVarint  = 0
	WireFixed64 = 1
	WireBytes   = 2
	WireFixed32 = 5
)

var (
	ErrTruncated = errors.New("pb: обрезанное сообщение")
	ErrWireType  = errors.New("pb: неподдерживаемый тип провода")
	ErrFieldZero = errors.New("pb: номер поля 0")
	ErrVarint    = errors.New("pb: переполнение varint")
)

// Field — одно поле сообщения. Для WireBytes значение в B, для остальных — в U.
type Field struct {
	Num  uint64
	Wire uint8
	U    uint64
	B    []byte
}

// Reader последовательно отдаёт поля сообщения.
type Reader struct {
	b   []byte
	off int
}

func NewReader(b []byte) *Reader { return &Reader{b: b} }

// Next возвращает следующее поле; в конце сообщения — io.EOF.
func (r *Reader) Next() (Field, error) {
	if r.off >= len(r.b) {
		return Field{}, io.EOF
	}
	key, err := r.varint()
	if err != nil {
		return Field{}, err
	}
	f := Field{Num: key >> 3, Wire: uint8(key & 7)}
	if f.Num == 0 {
		return Field{}, ErrFieldZero
	}
	switch f.Wire {
	case WireVarint:
		f.U, err = r.varint()
	case WireFixed64:
		err = r.fixed(&f, 8)
	case WireFixed32:
		err = r.fixed(&f, 4)
	case WireBytes:
		var n uint64
		if n, err = r.varint(); err != nil {
			return Field{}, err
		}
		if n > uint64(len(r.b)-r.off) {
			return Field{}, ErrTruncated
		}
		f.B = r.b[r.off : r.off+int(n)]
		r.off += int(n)
	default:
		return Field{}, ErrWireType
	}
	if err != nil {
		return Field{}, err
	}
	return f, nil
}

func (r *Reader) fixed(f *Field, n int) error {
	if len(r.b)-r.off < n {
		return ErrTruncated
	}
	var v uint64
	for i := n - 1; i >= 0; i-- {
		v = v<<8 | uint64(r.b[r.off+i])
	}
	f.U = v
	r.off += n
	return nil
}

func (r *Reader) varint() (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 70; shift += 7 {
		if r.off >= len(r.b) {
			return 0, ErrTruncated
		}
		c := r.b[r.off]
		r.off++
		if shift == 63 && c > 1 {
			return 0, ErrVarint
		}
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, ErrVarint
}

// Validate проверяет, что b целиком разбирается как protobuf.
func Validate(b []byte) error {
	r := NewReader(b)
	for {
		if _, err := r.Next(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Find возвращает первое поле с номером num. На битом сообщении возвращает то, что успело разобраться.
func Find(b []byte, num uint64) (Field, bool) {
	r := NewReader(b)
	for {
		f, err := r.Next()
		if err != nil {
			return Field{}, false
		}
		if f.Num == num {
			return f, true
		}
	}
}

// Bytes — содержимое поля num типа bytes/string/вложенное сообщение.
func Bytes(b []byte, num uint64) ([]byte, bool) {
	f, ok := Find(b, num)
	if !ok || f.Wire != WireBytes {
		return nil, false
	}
	return f.B, true
}

// Uint — значение поля num типа varint/fixed.
func Uint(b []byte, num uint64) (uint64, bool) {
	f, ok := Find(b, num)
	if !ok || f.Wire == WireBytes {
		return 0, false
	}
	return f.U, true
}

// Str — строковое поле num (копия, поэтому не держит исходный буфер).
func Str(b []byte, num uint64) (string, bool) {
	v, ok := Bytes(b, num)
	return string(v), ok
}

// Has — есть ли поле num любого типа.
func Has(b []byte, num uint64) bool {
	_, ok := Find(b, num)
	return ok
}

// Nums — номера верхнеуровневых полей по порядку появления (без повторов).
func Nums(b []byte) []uint64 {
	var out []uint64
	r := NewReader(b)
	for {
		f, err := r.Next()
		if err != nil {
			return out
		}
		seen := false
		for _, n := range out {
			if n == f.Num {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, f.Num)
		}
	}
}

// --- сборка ---

func AppendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func AppendUint(dst []byte, num, v uint64) []byte {
	return AppendVarint(AppendVarint(dst, num<<3|WireVarint), v)
}

func AppendBytes(dst []byte, num uint64, v []byte) []byte {
	dst = AppendVarint(dst, num<<3|WireBytes)
	dst = AppendVarint(dst, uint64(len(v)))
	return append(dst, v...)
}

func AppendString(dst []byte, num uint64, s string) []byte {
	dst = AppendVarint(dst, num<<3|WireBytes)
	dst = AppendVarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// AppendField дописывает поле как есть (тип провода и значение берутся из f): так поле, прочитанное Reader'ом,
// переносится в новое сообщение без потери типа (fixed32/fixed64 не превращаются в varint).
func AppendField(dst []byte, f Field) []byte {
	dst = AppendVarint(dst, f.Num<<3|uint64(f.Wire))
	switch f.Wire {
	case WireVarint:
		return AppendVarint(dst, f.U)
	case WireBytes:
		dst = AppendVarint(dst, uint64(len(f.B)))
		return append(dst, f.B...)
	case WireFixed32:
		return append(dst, byte(f.U), byte(f.U>>8), byte(f.U>>16), byte(f.U>>24))
	case WireFixed64:
		return append(dst, byte(f.U), byte(f.U>>8), byte(f.U>>16), byte(f.U>>24),
			byte(f.U>>32), byte(f.U>>40), byte(f.U>>48), byte(f.U>>56))
	}
	return dst
}

// AppendFixed64 дописывает поле fixed64 (в Quasar так лежат double).
func AppendFixed64(dst []byte, num, v uint64) []byte {
	return AppendField(dst, Field{Num: num, Wire: WireFixed64, U: v})
}
