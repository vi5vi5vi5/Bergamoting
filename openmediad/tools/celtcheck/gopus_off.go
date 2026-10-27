//go:build !gopus

package main

import "errors"

func decodeGopus([][]byte) ([]int16, error) {
	return nil, errors.New("gopus не включён в сборку (тег gopus)")
}
