// Package api — типы HTTP API агента ledd, общие для агента и клиентов.
//
//	GET  /api/state          State
//	GET  /api/events         поток State (Server-Sent Events) на каждое изменение
//	POST /api/ring           RingRequest — взять кольцо под управление и поменять зоны
//	POST /api/ring/release   отдать кольцо прошивке
//	POST /api/clock          ClockRequest — взять часы под управление и поменять сегменты
//	POST /api/clock/release  отдать часы прошивке
package api

type State struct {
	Ring     Ring     `json:"ring"`
	Clock    Clock    `json:"clock"`
	Firmware Firmware `json:"firmware"`
}

type Zone struct {
	Color      string `json:"color"`      // #rrggbb
	Brightness int    `json:"brightness"` // 0..255
	On         bool   `json:"on"`
}

type Ring struct {
	Owned  bool     `json:"owned"`  // кольцом управляем мы, а не прошивка
	Active bool     `json:"active"` // наш кадр сейчас горит; false — уступили анимации Алисы
	Zones  []Zone   `json:"zones"`  // что хотим показать
	Actual []string `json:"actual"` // что реально горит по зонам, #rrggbb
}

type Clock struct {
	Owned      bool            `json:"owned"`
	Active     bool            `json:"active"`
	Brightness int             `json:"brightness"` // 0..255, общая для всех сегментов
	Segments   map[string]bool `json:"segments"`   // что хотим показать
	Actual     map[string]int  `json:"actual"`     // яркость каждого сегмента сейчас
}

// Firmware — что сейчас рисует прошивка, по логу maind.
type Firmware struct {
	Ring      []string `json:"ring"`
	Clock     []string `json:"clock"`
	RingIdle  bool     `json:"ringIdle"`
	ClockIdle bool     `json:"clockIdle"`
}

// RingRequest — тело POST /api/ring. Незаданные поля не меняются;
// без Zones изменение применяется ко всем зонам.
//
//	{"color": "#ff0000"}                         все зоны красным
//	{"zones": [0, 3], "brightness": 40}          зоны 0 и 3 тусклее
//	{"zones": [5], "on": false}                  погасить зону 5
type RingRequest struct {
	Zones      []int   `json:"zones,omitempty"`
	Color      *string `json:"color,omitempty"`
	Brightness *int    `json:"brightness,omitempty"`
	On         *bool   `json:"on,omitempty"`
}

// ClockRequest — тело POST /api/clock. Применяется по порядку: Clear, Text, Segments, Brightness.
//
//	{"text": "12:34"}
//	{"segments": {"G0": true, "DH": false}}
//	{"clear": true, "brightness": 80}
type ClockRequest struct {
	Clear      bool            `json:"clear,omitempty"`
	Text       *string         `json:"text,omitempty"`
	Segments   map[string]bool `json:"segments,omitempty"`
	Brightness *int            `json:"brightness,omitempty"`
}

type Error struct {
	Error string `json:"error"`
}
