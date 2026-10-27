package agent

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"ledlab/internal/api"
)

//go:embed web
var web embed.FS

// Handler — HTTP API (см. пакет api) и страница с кнопками на "/".
func (a *Agent) Handler() http.Handler {
	static, _ := fs.Sub(web, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.State())
	})
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("POST /api/ring", func(w http.ResponseWriter, r *http.Request) {
		var req api.RingRequest
		if readJSON(w, r, &req) {
			a.reply(w, a.SetRing(req))
		}
	})
	mux.HandleFunc("POST /api/ring/release", func(w http.ResponseWriter, r *http.Request) {
		a.ReleaseRing()
		a.reply(w, nil)
	})
	mux.HandleFunc("POST /api/clock", func(w http.ResponseWriter, r *http.Request) {
		var req api.ClockRequest
		if readJSON(w, r, &req) {
			a.reply(w, a.SetClock(req))
		}
	})
	mux.HandleFunc("POST /api/clock/release", func(w http.ResponseWriter, r *http.Request) {
		a.ReleaseClock()
		a.reply(w, nil)
	})
	return allowCORS(mux)
}

// reply отвечает ошибкой или текущим состоянием.
func (a *Agent) reply(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.State())
}

// handleEvents отдаёт состояние потоком Server-Sent Events: сразу и на каждое изменение.
func (a *Agent) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	ch := a.subscribe()
	defer a.unsubscribe(ch)

	send := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	first, _ := json.Marshal(a.State())
	if !send("data: %s\n\n", first) {
		return
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if !send("data: %s\n\n", msg) {
				return
			}
		case <-ping.C:
			if !send(": ping\n\n") {
				return
			}
		}
	}
}

// readJSON разбирает тело запроса; пустое тело — допустимо.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "тело запроса: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// allowCORS разрешает звать API из браузерных приложений с других адресов.
func allowCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
