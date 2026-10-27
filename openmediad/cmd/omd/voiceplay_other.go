//go:build !linux

package main

import "errors"

func voicePlay([]string) error {
	return errors.New("voice-play работает только на Linux (на колонке)")
}
