package dbg

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"
	"time"
)

//go:embed page.html
var page []byte

// Server — отладочный HTTP-порт шима. Только чтение: ничем на колонке управлять через него нельзя.
//
//	GET /                веб-страница (живой журнал с пояснениями)
//	GET /api/log         записи: ?since=ID&limit=N&format=ndjson
//	GET /api/stream      поток SSE: накопленное с ?since=ID, затем новые записи
//	GET /api/state       JSON с состоянием шима: аптайм, память, плееры (State)
//	GET /debug/pprof/    профилировщик Go (куда уходит процессор и память шима):
//	                     go tool pprof http://адрес:6701/debug/pprof/profile?seconds=15
type Server struct {
	Hub *Hub
	// State возвращает произвольный JSON-совместимый снимок состояния (плееры, движок, статистика).
	State func() any
	// CORS разрешает чтение из страницы, открытой с другого источника (локальный html-файл). По умолчанию выключено:
	// журнал содержит распознанные фразы и названия треков, и читать его чужой веб-странице незачем.
	CORS bool
}

// Handler собирает маршруты.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	mux.HandleFunc("/api/log", s.cors(s.log))
	mux.HandleFunc("/api/stream", s.cors(s.stream))
	mux.HandleFunc("/api/state", s.cors(s.state))
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func (s *Server) cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.CORS {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "только GET", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func uintParam(r *http.Request, name string) uint64 {
	v, _ := strconv.ParseUint(r.URL.Query().Get(name), 10, 64)
	return v
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	es := s.Hub.Since(uintParam(r, "since"), int(uintParam(r, "limit")))
	if r.URL.Query().Get("format") == "ndjson" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="omd-log.ndjson"`)
		enc := json.NewEncoder(w)
		for _, e := range es {
			enc.Encode(e)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if es == nil {
		es = []Entry{}
	}
	json.NewEncoder(w).Encode(es)
}

// maxStreams ограничивает число одновременных подписчиков SSE: колонка слабая, а страниц обычно одна-две.
const maxStreams = 6

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "поток не поддерживается", http.StatusInternalServerError)
		return
	}
	if s.Hub.Subscribers() >= maxStreams {
		http.Error(w, "слишком много слушателей", http.StatusServiceUnavailable)
		return
	}
	ch, unsub := s.Hub.Subscribe()
	defer unsub()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	last := uintParam(r, "since")
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if v, err := strconv.ParseUint(h, 10, 64); err == nil && v > last {
			last = v
		}
	}
	send := func(e Entry) bool {
		if e.ID <= last {
			return true
		}
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, b); err != nil {
			return false
		}
		last = e.ID
		return true
	}
	for _, e := range s.Hub.Since(last, 0) { // накопленное: подписка уже оформлена, значит, ничего не потеряется
		if !send(e) {
			return
		}
	}
	fl.Flush()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send(e) {
				return
			}
			fl.Flush()
		case <-keep.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := map[string]any{
		"now_ms":     time.Now().UnixMilli(),
		"uptime_s":   int64(time.Since(s.Hub.Start).Seconds()),
		"goroutines": runtime.NumGoroutine(),
		"heap_kb":    ms.HeapAlloc >> 10,
		"sys_kb":     ms.Sys >> 10,
		"log_counts": s.Hub.Counts(),
		"viewers":    s.Hub.Subscribers(),
	}
	if s.State != nil {
		out["shim"] = s.State()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// Listen запускает сервер на addr (например «0.0.0.0:6701») и работает, пока не отменён ctx.
func (s *Server) Listen(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(c)
		srv.Close()
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
