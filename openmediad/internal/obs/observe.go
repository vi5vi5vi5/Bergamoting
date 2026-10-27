package obs

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"openmediad/internal/wire"
)

// Target — сервис шины, к которому подключается наблюдатель.
type Target struct {
	Name string
	Addr string
}

// Observer подключается к сервисам как обычный клиент и печатает то, что сервис шлёт подписчикам.
// Он ничего не отправляет. Важно: так видно только рассылки сервиса, а не команды, которые идут между
// другими клиентами (например, AudioCommand от platformd к mediad) — для них нужен pcap или шим.
type Observer struct {
	Targets []Target
	Opt     Opt
	Out     io.Writer
	Stats   time.Duration // период сводки по типам кадров; 0 — без сводки
	Mono    func() int64  // если задано, к времени строки добавляется монотонная метка CLOCK_MONOTONIC
	Bind    net.IP        // если задано, исходящие соединения идут с этого локального адреса (для проверки правил NAT)
	// OnLine, если задан, получает строки вместо записи в Out: who — имя сервиса, text — описание без времени.
	OnLine func(who, text string)

	mu     sync.Mutex
	counts map[string]int
}

func (o *Observer) Run(ctx context.Context) error {
	o.counts = map[string]int{}
	var wg sync.WaitGroup
	for _, t := range o.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.watch(ctx, t)
		}()
	}
	if o.Stats > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.statsLoop(ctx)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (o *Observer) watch(ctx context.Context, t Target) {
	delay := time.Second
	for ctx.Err() == nil {
		d := net.Dialer{Timeout: 2 * time.Second}
		if o.Bind != nil {
			d.LocalAddr = &net.TCPAddr{IP: o.Bind}
		}
		conn, err := d.DialContext(ctx, "tcp", t.Addr)
		if err != nil {
			o.logf(t.Name, "нет соединения: %v (повтор через %s)", err, delay)
			if !sleep(ctx, delay) {
				return
			}
			if delay < 10*time.Second {
				delay *= 2
			}
			continue
		}
		delay = time.Second
		o.logf(t.Name, "подключён к %s", t.Addr)
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		err = o.read(t, conn)
		stop()
		conn.Close()
		if ctx.Err() != nil {
			return
		}
		o.logf(t.Name, "соединение закрыто: %v", err)
		if !sleep(ctx, time.Second) {
			return
		}
	}
}

func (o *Observer) read(t Target, conn net.Conn) error {
	fr := wire.NewFrameReader(conn)
	for {
		frame, err := fr.Next()
		if err != nil {
			return err
		}
		m, err := wire.Unwrap(frame)
		if err != nil {
			o.logf(t.Name, "кадр без конверта, %d Б: %v", len(frame), err)
			continue
		}
		o.mu.Lock()
		o.counts[t.Name+" "+m.Type.String()]++
		o.mu.Unlock()
		if line, show := Describe(t.Name, m, o.Opt); show {
			o.logf(t.Name, "%s", line)
		}
	}
}

func (o *Observer) statsLoop(ctx context.Context) {
	tick := time.NewTicker(o.Stats)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		o.mu.Lock()
		keys := make([]string, 0, len(o.counts))
		for k := range o.counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := ""
		for _, k := range keys {
			s += fmt.Sprintf(" %s=%d", k, o.counts[k])
		}
		clear(o.counts)
		o.mu.Unlock()
		if s != "" {
			o.logf("stats", "за %s:%s", o.Stats, s)
		}
	}
}

func (o *Observer) logf(who, format string, a ...any) {
	if o.OnLine != nil {
		o.OnLine(who, fmt.Sprintf(format, a...))
		return
	}
	ts := time.Now().UTC().Format("15:04:05.000")
	if o.Mono != nil {
		ts += fmt.Sprintf(" m%.4f", float64(o.Mono())/1e9)
	}
	line := fmt.Sprintf("%s %-11s %s\n", ts, who, fmt.Sprintf(format, a...))
	o.mu.Lock()
	io.WriteString(o.Out, line)
	o.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
