//go:build !linux

package main

func memLine() string { return "" }

func printSelfMem() {}
