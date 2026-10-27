package sink

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Вместо настоящего aplay тест запускает собственный бинарник в роли «поддельного aplay»: поведение задаёт
// переменная OMD_FAKE_APLAY, а аргументы и принятый PCM записываются рядом с OMD_FAKE_OUT.
func TestMain(m *testing.M) {
	if mode := os.Getenv("OMD_FAKE_APLAY"); mode != "" {
		fakeAplay(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeAplay(mode string) {
	out := os.Getenv("OMD_FAKE_OUT")
	os.WriteFile(out+".args", []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	switch mode {
	case "copy":
		data, _ := io.ReadAll(os.Stdin)
		os.WriteFile(out+".pcm", data, 0o600)
	case "slow": // после конца потока ещё «доигрывает»
		io.Copy(io.Discard, os.Stdin)
		time.Sleep(400 * time.Millisecond)
	case "fail":
		fmt.Fprint(os.Stderr, "aplay: main:850: audio open error: Device or resource busy")
		os.Exit(1)
	case "hang": // игнорирует конец потока; умирает только от сигнала
		time.Sleep(30 * time.Second)
	case "block": // читает, пока не прервут
		io.Copy(io.Discard, os.Stdin)
		time.Sleep(30 * time.Second)
	}
}

func fake(t *testing.T, mode string) (Config, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fake")
	t.Setenv("OMD_FAKE_APLAY", mode)
	t.Setenv("OMD_FAKE_OUT", out)
	return Config{Path: os.Args[0]}, out
}

func TestArgs(t *testing.T) {
	got := Config{}.Args()
	want := []string{"-q", "-D", "default", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "1"}
	if !slices.Equal(got, want) {
		t.Fatalf("по умолчанию: %v", got)
	}
	got = Config{Device: "bluealsa:DEV=AA:BB", Rate: 44100, Channels: 2, BufferUS: 100000, PeriodUS: 20000, StartDelayUS: 1}.Args()
	want = []string{"-q", "-D", "bluealsa:DEV=AA:BB", "-t", "raw", "-f", "S16_LE", "-r", "44100", "-c", "2",
		"-B", "100000", "-F", "20000", "-R", "1"}
	if !slices.Equal(got, want) {
		t.Fatalf("с опциями: %v", got)
	}
}

func TestStreamsAllDataAndFinishes(t *testing.T) {
	cfg, out := fake(t, "copy")
	cfg.StartDelayUS = 1
	p, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for i := 0; i < 50; i++ {
		piece := bytes.Repeat([]byte{byte(i)}, 2000)
		want = append(want, piece...)
		if _, err := p.Write(piece); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Finish(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out + ".pcm")
	if !bytes.Equal(got, want) {
		t.Fatalf("aplay получил %d Б, ждали %d Б", len(got), len(want))
	}
	args, _ := os.ReadFile(out + ".args")
	if string(args) != strings.Join(cfg.Args(), "\n") {
		t.Fatalf("аргументы: %q", args)
	}
}

func TestFinishWaitsForSlowDrain(t *testing.T) {
	cfg, _ := fake(t, "slow")
	p, _ := Start(cfg)
	t0 := time.Now()
	if err := p.Finish(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el < 350*time.Millisecond {
		t.Fatalf("Finish вернулся за %v, не дождавшись доигрывания", el)
	}
}

func TestFailureIsReportedWithStderr(t *testing.T) {
	cfg, _ := fake(t, "fail")
	p, _ := Start(cfg)
	<-p.Done()
	err := p.Finish(time.Second)
	if err == nil || !strings.Contains(err.Error(), "Device or resource busy") {
		t.Fatalf("ждали ошибку со stderr aplay, получили %v", err)
	}
	if _, err := p.Write([]byte{1}); err == nil {
		t.Fatal("запись в завершившийся aplay должна давать ошибку")
	}
}

func TestStartMissingBinary(t *testing.T) {
	if _, err := Start(Config{Path: filepath.Join(t.TempDir(), "нет-такого-aplay")}); err == nil {
		t.Fatal("ждали ошибку запуска")
	}
}

func TestFinishTimeoutUsesInterruptNeverKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT процессу на Windows не посылается")
	}
	cfg, _ := fake(t, "hang")
	p, _ := Start(cfg)
	t0 := time.Now()
	err := p.Finish(200 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "interrupt") || strings.Contains(err.Error(), "kill") {
		t.Fatalf("процесс должен умереть от SIGINT, получили %v", err)
	}
	if time.Since(t0) > 4*time.Second {
		t.Fatal("слишком долго")
	}
}

func TestAbortStopsPlaybackAtOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT процессу на Windows не посылается")
	}
	cfg, _ := fake(t, "block")
	p, _ := Start(cfg)
	p.Write(bytes.Repeat([]byte{1}, 4096))
	t0 := time.Now()
	err := p.Abort(2 * time.Second)
	if errors.Is(err, ErrStuck) || err == nil {
		t.Fatalf("Abort должен остановить процесс, получили %v", err)
	}
	if time.Since(t0) > time.Second {
		t.Fatal("Abort должен быть быстрым")
	}
}
