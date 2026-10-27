// Package sink — вывод PCM на колонку через системный aplay.
//
// Почему aplay, а не собственный вызов ALSA: libasound на колонке 32-битная, как и mediad, а общий смеситель
// dmix хранит состояние в разделяемой памяти с типами, размер которых зависит от разрядности процесса.
// 64-битный клиент в этом смесителе, вероятно, испортил бы его для штатного стека (на колонке не проверяли).
// Родной 32-битный aplay безопасно подключается к dmix и уживается со штатной музыкой (проверено, PLAN §13).
//
// ВАЖНО: aplay никогда не убивается SIGKILL. Процесс, оборванный посреди записи, может оставить dmix
// в состоянии, когда замолкает вся колонка вместе с Алисой. Остановка — закрытием stdin (доиграет остаток)
// или SIGINT (aplay сам вызовет snd_pcm_abort и закроет PCM).
package sink

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// ErrStuck — aplay не завершился ни после закрытия stdin, ни после SIGINT. Процесс оставлен жить: SIGKILL
// опаснее зависшего клиента.
var ErrStuck = errors.New("sink: aplay не завершается; SIGKILL не используется")

// Config — параметры aplay. Нулевые поля означают значения по умолчанию.
type Config struct {
	Path     string // по умолчанию /usr/bin/aplay
	Device   string // по умолчанию "default" (на колонке это общий dmix)
	Rate     int    // по умолчанию 48000
	Channels int    // по умолчанию 1: преобразование в стерео делает plug ALSA
	BufferUS int    // 0 — по умолчанию aplay (просит 500 мс, но у dmix реальный буфер ≈160 мс)
	PeriodUS int    // 0 — по умолчанию aplay

	// StartDelayUS — aplay -R. 0 (умолчание): PCM стартует, когда в буфере клиента (у dmix он 160 мс) наберётся
	// полный буфер. Если данные приходят быстрее реального времени, как целый чанк голоса из декодера, это
	// незаметно (замер: 15,8 против 16,1 мс до запуска PCM). На потоке, идущем в реальном времени, умолчание
	// добавляет ≈100 мс к старту, но даёт запас на дрожание (0 underrun). >0: старт после стольких микросекунд
	// данных (1 — сразу), но без запаса поток реального времени даёт underrun (замер: 5–8 за 1 с).
	StartDelayUS int
}

func (c Config) withDefaults() Config {
	if c.Path == "" {
		c.Path = "/usr/bin/aplay"
	}
	if c.Device == "" {
		c.Device = "default"
	}
	if c.Rate == 0 {
		c.Rate = 48000
	}
	if c.Channels == 0 {
		c.Channels = 1
	}
	return c
}

// Args — аргументы командной строки aplay (удобно для журнала и тестов).
func (c Config) Args() []string {
	c = c.withDefaults()
	a := []string{"-q", "-D", c.Device, "-t", "raw", "-f", "S16_LE",
		"-r", strconv.Itoa(c.Rate), "-c", strconv.Itoa(c.Channels)}
	if c.BufferUS > 0 {
		a = append(a, "-B", strconv.Itoa(c.BufferUS))
	}
	if c.PeriodUS > 0 {
		a = append(a, "-F", strconv.Itoa(c.PeriodUS))
	}
	if c.StartDelayUS > 0 {
		a = append(a, "-R", strconv.Itoa(c.StartDelayUS))
	}
	return a
}

// Player — запущенный aplay, читающий PCM S16LE из своего stdin.
type Player struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	done    chan struct{}
	waitErr error
	stderr  tail
}

// Start запускает aplay. Возврат из Start означает только «процесс создан»; готовность PCM — позже.
func Start(cfg Config) (*Player, error) {
	cfg = cfg.withDefaults()
	p := &Player{done: make(chan struct{})}
	p.cmd = exec.Command(cfg.Path, cfg.Args()...)
	p.cmd.Stderr = &p.stderr
	in, err := p.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	p.in = in
	if err := p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("sink: запуск %s: %w", cfg.Path, err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Write передаёт PCM aplay. Блокируется, когда буфер pipe и PCM заполнены, — это и есть темп воспроизведения.
func (p *Player) Write(b []byte) (int, error) { return p.in.Write(b) }

// Done закрывается, когда процесс aplay завершился.
func (p *Player) Done() <-chan struct{} { return p.done }

// Stderr — последние строки stderr aplay (для сообщений об ошибках).
func (p *Player) Stderr() string { return p.stderr.String() }

// Finish закрывает stdin, чтобы aplay доиграл остаток, и ждёт завершения. Если за timeout он не завершился,
// посылает SIGINT (aplay закрывает PCM корректно) и ждёт ещё grace; иначе возвращает ErrStuck.
func (p *Player) Finish(timeout time.Duration) error {
	p.in.Close()
	select {
	case <-p.done:
		return p.result()
	case <-time.After(timeout):
	}
	return p.interrupt(2 * time.Second)
}

// Abort обрывает воспроизведение сразу (SIGINT → snd_pcm_abort): для «перебили, замолчи».
func (p *Player) Abort(grace time.Duration) error {
	p.in.Close()
	return p.interrupt(grace)
}

func (p *Player) interrupt(grace time.Duration) error {
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		select {
		case <-p.done:
			return p.result()
		default:
			return fmt.Errorf("sink: SIGINT: %w", err)
		}
	}
	select {
	case <-p.done:
		return p.result()
	case <-time.After(grace):
		return ErrStuck
	}
}

func (p *Player) result() error {
	if p.waitErr == nil {
		return nil
	}
	if s := p.stderr.String(); s != "" {
		return fmt.Errorf("sink: aplay: %w: %s", p.waitErr, s)
	}
	return fmt.Errorf("sink: aplay: %w", p.waitErr)
}

// tail хранит последние 512 байт вывода.
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 512 {
		t.b = t.b[len(t.b)-512:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
