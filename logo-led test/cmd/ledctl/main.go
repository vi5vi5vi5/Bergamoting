// ledctl — управление агентом ledd на колонке с компьютера по SSH.
//
//	go run ./cmd/ledctl deploy    собрать ledd под колонку, залить и перезапустить
//	go run ./cmd/ledctl status    запущен ли агент, сколько ест памяти, кто управляет подсветкой
//	go run ./cmd/ledctl start | stop | logs
//
// Адрес колонки: -host 192.168.1.12 -ssh 2222; порт API агента: -http 8765.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"ledlab/internal/api"
)

const (
	remoteBin = "/data/opt/bin/ledd"
	remoteLog = "/tmp/ledd.log" // /tmp в RAM: не изнашиваем флеш логами
)

// На колонке нет timeout и дробного sleep во всех оболочках, поэтому ждём через usleep.
const stopScript = `killall ledd 2>/dev/null
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  pidof ledd >/dev/null || exit 0
  usleep 100000
done
killall -9 ledd 2>/dev/null; true`

type speaker struct {
	host     string
	sshPort  int
	httpPort int
	client   *ssh.Client
}

func main() {
	sp := &speaker{}
	flag.StringVar(&sp.host, "host", "192.168.1.12", "IP колонки")
	flag.IntVar(&sp.sshPort, "ssh", 2222, "порт SSH")
	flag.IntVar(&sp.httpPort, "http", 8765, "порт HTTP API агента")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "использование: ledctl [флаги] deploy|start|stop|status|logs")
		flag.PrintDefaults()
	}
	flag.Parse()

	commands := map[string]func(*speaker) error{
		"deploy": deploy, "start": start, "stop": stop, "status": status, "logs": logs,
	}
	cmd, ok := commands[flag.Arg(0)]
	if !ok {
		flag.Usage()
		os.Exit(2)
	}
	if err := sp.connect(); err != nil {
		fmt.Fprintln(os.Stderr, "SSH:", err)
		os.Exit(1)
	}
	defer sp.client.Close()
	if err := cmd(sp); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func deploy(sp *speaker) error {
	bin, err := build()
	if err != nil {
		return err
	}
	fmt.Printf("собран ledd: %.1f МБ\n", float64(len(bin))/1e6)
	if err := sp.upload(remoteBin+".new", bin); err != nil {
		return fmt.Errorf("заливка: %w", err)
	}
	if err := stop(sp); err != nil {
		return err
	}
	if _, err := sp.run("mv " + remoteBin + ".new " + remoteBin); err != nil {
		return err
	}
	return start(sp)
}

// build собирает cmd/ledd статическим бинарником под arm64 (ядро колонки 64-битное).
func build() ([]byte, error) {
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(gomod)))
	out := filepath.Join(root, "bin", "ledd")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/ledd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("сборка: %w", err)
	}
	return os.ReadFile(out)
}

func start(sp *speaker) error {
	if out, _ := sp.run("pidof ledd"); strings.TrimSpace(out) != "" {
		fmt.Println("ledd уже запущен, pid", strings.TrimSpace(out))
	} else if _, err := sp.run(fmt.Sprintf("setsid %s -listen :%d >%s 2>&1 </dev/null &",
		remoteBin, sp.httpPort, remoteLog)); err != nil {
		return err
	}
	if err := sp.waitHTTP(5 * time.Second); err != nil {
		out, _ := sp.run("tail -20 " + remoteLog)
		return fmt.Errorf("агент не отвечает по HTTP: %w\n%s", err, out)
	}
	fmt.Printf("готово: http://%s:%d\n", sp.host, sp.httpPort)
	return nil
}

func stop(sp *speaker) error {
	_, err := sp.run(stopScript)
	return err
}

func status(sp *speaker) error {
	out, _ := sp.run(`pid=$(pidof ledd) && echo "pid $pid, $(grep VmRSS /proc/$pid/status)"`)
	if strings.TrimSpace(out) == "" {
		fmt.Println("ledd не запущен")
		return nil
	}
	fmt.Print(out)
	resp, err := http.Get(sp.url("/api/state"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var st api.State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return err
	}
	who := func(owned, active bool) string {
		switch {
		case !owned:
			return "прошивка"
		case active:
			return "ledd"
		default:
			return "ledd, ждёт конца анимации Алисы"
		}
	}
	fmt.Printf("кольцо: %s, горит %v\n", who(st.Ring.Owned, st.Ring.Active), st.Ring.Actual)
	fmt.Printf("часы: %s\n", who(st.Clock.Owned, st.Clock.Active))
	fmt.Printf("Алиса: кольцо %v, часы %v\n", st.Firmware.Ring, st.Firmware.Clock)
	fmt.Println(sp.url("/"))
	return nil
}

func logs(sp *speaker) error {
	out, err := sp.run("cat " + remoteLog)
	fmt.Print(out)
	return err
}

func (sp *speaker) connect() error {
	var err error
	sp.client, err = ssh.Dial("tcp", fmt.Sprintf("%s:%d", sp.host, sp.sshPort), &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.Password("")}, // dropbear -B: вход root с пустым паролем
		// Ключ хоста не проверяем: колонка в домашней сети, а ключ меняется после перепрошивки.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	return err
}

func (sp *speaker) run(cmd string) (string, error) {
	s, err := sp.client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	return string(out), err
}

// upload заливает файл через stdin SSH-сессии: scp и sftp на колонке нет.
func (sp *speaker) upload(path string, data []byte) error {
	s, err := sp.client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	s.Stdin = bytes.NewReader(data)
	out, err := s.CombinedOutput(fmt.Sprintf("cat > %s && chmod +x %s", path, path))
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (sp *speaker) url(path string) string {
	return fmt.Sprintf("http://%s:%d%s", sp.host, sp.httpPort, path)
}

func (sp *speaker) waitHTTP(limit time.Duration) error {
	client := http.Client{Timeout: time.Second}
	deadline := time.Now().Add(limit)
	for {
		resp, err := client.Get(sp.url("/api/state"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = errors.New(resp.Status)
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
