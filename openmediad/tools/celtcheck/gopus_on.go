//go:build gopus

package main

import gopus "github.com/thesyncim/gopus"

func decodeGopus(pkts [][]byte) ([]int16, error) {
	d, err := gopus.NewDecoder(gopus.DefaultDecoderConfig(48000, 1))
	if err != nil {
		return nil, err
	}
	buf := make([]int16, 5760)
	var out []int16
	for i, p := range pkts {
		n, err := d.DecodeInt16(p, buf)
		if err != nil {
			return nil, fmt.Errorf("пакет %d: %w", i, err)
		}
		out = append(out, buf[:n]...)
	}
	return out, nil
}
