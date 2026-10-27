// ledd — агент на колонке: управляет кольцом и часами, отдаёт HTTP API
// и страницу с кнопками. Собирается и заливается командой `ledctl deploy`.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"ledlab/internal/agent"
	"ledlab/internal/hw"
)

func main() {
	listen := flag.String("listen", ":8765", "адрес HTTP API")
	flag.Parse()
	debug.SetMemoryLimit(32 << 20) // свободной памяти на колонке ~100 МБ на всех

	ring, err := hw.OpenRing()
	if err != nil {
		log.Fatalf("кольцо: %v", err)
	}
	defer ring.Close()
	clock, err := hw.OpenClock()
	if err != nil {
		log.Fatalf("часы: %v", err)
	}
	defer clock.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ag := agent.New(ring, clock)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           ag.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// Запросы наследуют ctx: при остановке потоки /api/events закроются сами.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("ledd слушает %s", *listen)

	ag.Run(ctx)
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	log.Print("ledd остановлен")
}
