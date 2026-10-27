//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
)

// Прибор не должен искажать то, что измеряет: один буфер выделяется заранее (в базовую линию он попадает
// целиком), разбор ручной, без строк и срезов на каждый вызов.
var memScratch = make([]byte, 128<<10)

func slurp(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	n := 0
	for n < len(memScratch) {
		k, err := f.Read(memScratch[n:])
		n += k
		if err != nil || k == 0 {
			break
		}
	}
	return memScratch[:n]
}

// field возвращает число после ключа в начале строки (формат «Key:   123 kB»).
func field(data []byte, key string) (v int, ok bool) {
	k := []byte(key)
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		if !bytes.HasPrefix(line, k) {
			continue
		}
		p := len(k)
		for p < len(line) && (line[p] == ' ' || line[p] == '\t') {
			p++
		}
		for ; p < len(line) && line[p] >= '0' && line[p] <= '9'; p++ {
			v = v*10 + int(line[p]-'0')
		}
		return v, true
	}
	return 0, false
}

func sumField(data []byte, key string) (sum int) {
	k := []byte(key)
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		if !bytes.HasPrefix(line, k) {
			continue
		}
		v := 0
		for p := len(k); p < len(line); p++ {
			if line[p] >= '0' && line[p] <= '9' {
				v = v*10 + int(line[p]-'0')
			}
		}
		sum += v
	}
	return sum
}

// memLine — компактная строка о памяти процесса (КБ). RSS и пик читаются до smaps.
func memLine() string {
	st := slurp("/proc/self/status")
	if st == nil {
		return ""
	}
	rss, _ := field(st, "VmRSS:")
	hwm, _ := field(st, "VmHWM:")
	thr, _ := field(st, "Threads:")
	sm := slurp("/proc/self/smaps")
	return fmt.Sprintf("RSS %d КБ, пик %d КБ, Pss %d КБ, Private_Dirty %d КБ, потоков %d",
		rss, hwm, sumField(sm, "Pss:"), sumField(sm, "Private_Dirty:"), thr)
}

func printSelfMem() {
	if l := memLine(); l != "" {
		fmt.Println("\nпамять процесса при выходе:", l)
	}
}
