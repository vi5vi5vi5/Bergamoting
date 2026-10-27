// sshbench — эксперимент: сколько стоит одна LED-команда, если управлять
// колонкой с Windows только по SSH, без своего сервиса на колонке.
//
// Запуск:  go run ./experiments/sshbench -addr 192.168.1.12:2222
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	addr := flag.String("addr", "192.168.1.12:2222", "адрес колонки")
	n := flag.Int("n", 40, "команд в каждом тесте")
	flag.Parse()

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.Password("")}, // dropbear -B: пустой пароль
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	t := time.Now()
	client, err := ssh.Dial("tcp", *addr, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()
	fmt.Printf("подключение SSH: %v\n", time.Since(t))

	saved, _ := run(client, "dd if=/dev/fb0 bs=18 count=1 2>/dev/null | od -An -v -to1 | tr -d '\\n' | tr -s ' ' '\\\\'")
	defer run(client, "printf '"+strings.TrimSpace(saved)+"' > /dev/fb0")

	// 1) Новая SSH-сессия на каждую команду — так делает «наивный» клиент.
	var perSession []time.Duration
	for i := 0; i < *n; i++ {
		t := time.Now()
		if _, err := run(client, "printf '"+frame(i)+"' > /dev/fb0"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		perSession = append(perSession, time.Since(t))
	}
	report("новая сессия на команду", perSession)

	// 2) Одна долгоживущая оболочка, команды идут в её stdin.
	sess, err := client.NewSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer sess.Close()
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Start("sh"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out := bufio.NewReader(stdout)
	var persistent []time.Duration
	for i := 0; i < *n; i++ {
		t := time.Now()
		fmt.Fprintf(stdin, "printf '%s' > /dev/fb0; echo ok\n", frame(i))
		if _, err := out.ReadString('\n'); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		persistent = append(persistent, time.Since(t))
	}
	report("одна оболочка, команды в stdin", persistent)
}

// frame — кадр для printf: все 6 зон одним цветом, цвет меняется по кругу.
func frame(i int) string {
	colors := []string{`\0\0\100`, `\0\100\0`, `\100\0\0`} // BGR: красный, зелёный, синий
	return strings.Repeat(colors[i%len(colors)], 6)
}

func run(c *ssh.Client, cmd string) (string, error) {
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	return string(out), err
}

func report(name string, d []time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	fmt.Printf("%s: min %v  p50 %v  p95 %v  max %v\n", name,
		d[0].Round(100*time.Microsecond), d[len(d)/2].Round(100*time.Microsecond),
		d[len(d)*95/100].Round(100*time.Microsecond), d[len(d)-1].Round(100*time.Microsecond))
}
