// Package agent — логика ledd: хранит, что пользователь хочет видеть на кольце
// и часах, рисует это и уступает прошивке, пока Алиса показывает свои анимации.
//
// Прошивка не знает про нас и просто пишет в те же устройства. Поэтому агент
// каждые 50 мс перечитывает кольцо и часы: если там не то, что мы записали,
// значит рисует прошивка. Ждём, пока по логу maind она вернётся в покой и кадр
// перестанет меняться, и рисуем своё заново.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"ledlab/internal/api"
	"ledlab/internal/hw"
	"ledlab/internal/quasar"
)

const (
	pollEvery = 50 * time.Millisecond
	// Столько кадр должен не меняться после прошивки, прежде чем рисовать своё:
	// у кольца это затухание в idle.led, у часов — запись сегментов одного за другим.
	ringSettle             = 300 * time.Millisecond
	clockSettle            = 100 * time.Millisecond
	defaultClockBrightness = 40
)

type zone struct {
	color  hw.RGB
	bright uint8
	on     bool
}

type ringState struct {
	owned    bool // пользователь взял кольцо под управление
	active   bool // наш кадр сейчас горит
	zones    [hw.RingZones]zone
	written  hw.RingFrame // что мы записали последним
	actual   hw.RingFrame // что сейчас в fb0
	firmware hw.RingFrame // последний кадр, нарисованный не нами
	changed  time.Time    // когда кадр последний раз менял кто-то кроме нас
}

func (r *ringState) frame() (fr hw.RingFrame) {
	for i, z := range r.zones {
		if z.on {
			fr[i] = z.color.Scale(z.bright)
		}
	}
	return fr
}

type clockState struct {
	owned    bool
	active   bool
	segs     [hw.ClockSegmentCount]bool
	bright   uint8
	written  hw.ClockFrame
	actual   hw.ClockFrame
	firmware hw.ClockFrame
	changed  time.Time
}

func (c *clockState) frame() (fr hw.ClockFrame) {
	for i, on := range c.segs {
		if on {
			fr[i] = c.bright
		}
	}
	return fr
}

type Agent struct {
	ring  *hw.Ring
	clock *hw.Clock

	mu    sync.Mutex
	anim  quasar.Anim
	r     ringState
	c     clockState
	dirty bool // состояние изменилось с последней рассылки подписчикам
	subs  map[chan []byte]struct{}
}

func New(ring *hw.Ring, clock *hw.Clock) *Agent {
	a := &Agent{ring: ring, clock: clock, subs: map[chan []byte]struct{}{}}
	for i := range a.r.zones {
		a.r.zones[i] = zone{color: hw.RGB{R: 255, G: 255, B: 255}, bright: 255}
	}
	a.c.bright = defaultClockBrightness
	return a
}

// Run следит за железом и логом прошивки, пока не отменят ctx.
// На выходе отдаёт кольцо и часы прошивке.
func (a *Agent) Run(ctx context.Context) {
	go quasar.WatchAnims(ctx, quasar.MaindLog, 100*time.Millisecond, a.setAnim)
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	pub := time.NewTicker(100 * time.Millisecond)
	defer pub.Stop()
	for {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			a.releaseRing()
			a.releaseClock()
			a.mu.Unlock()
			return
		case now := <-poll.C:
			a.syncRing(now)
			a.syncClock(now)
		case <-pub.C:
			a.publish()
		}
	}
}

func (a *Agent) setAnim(an quasar.Anim) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.anim = an
	a.dirty = true
}

func (a *Agent) settled(changed, now time.Time, settle time.Duration) bool {
	return now.Sub(changed) >= settle && now.Sub(a.anim.At) >= settle
}

// ---- кольцо ----

func (a *Agent) syncRing(now time.Time) {
	actual, err := a.ring.Read()
	if err != nil {
		log.Printf("кольцо: чтение: %v", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := &a.r
	if actual != r.actual {
		r.actual = actual
		a.dirty = true
		if !r.active || actual != r.written {
			r.changed, r.firmware = now, actual
		}
	}
	if !r.owned {
		return
	}
	if r.active && actual != r.written {
		r.active = false // прошивка нарисовала поверх нас — пусть доиграет
		a.dirty = true
	}
	if !r.active {
		a.applyRing(now)
	}
}

// applyRing рисует наш кадр, если он уже горит (обновить) или прошивка освободила кольцо.
func (a *Agent) applyRing(now time.Time) {
	r := &a.r
	if !r.active && !(a.anim.RingIdle() && a.settled(r.changed, now, ringSettle)) {
		return
	}
	fr := r.frame()
	if err := a.ring.Write(fr); err != nil {
		log.Printf("кольцо: запись: %v", err)
		return
	}
	r.written, r.actual, r.active = fr, fr, true
	a.dirty = true
}

func (a *Agent) SetRing(req api.RingRequest) error {
	var color hw.RGB
	if req.Color != nil {
		c, err := hw.ParseHex(*req.Color)
		if err != nil {
			return err
		}
		color = c
	}
	if req.Brightness != nil && (*req.Brightness < 0 || *req.Brightness > 255) {
		return fmt.Errorf("яркость %d: нужно 0..255", *req.Brightness)
	}
	zones := req.Zones
	if len(zones) == 0 {
		zones = []int{0, 1, 2, 3, 4, 5}
	}
	for _, i := range zones {
		if i < 0 || i >= hw.RingZones {
			return fmt.Errorf("зона %d: есть только 0..%d", i, hw.RingZones-1)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, i := range zones {
		z := &a.r.zones[i]
		if req.Color != nil {
			z.color = color
		}
		if req.Brightness != nil {
			z.bright = uint8(*req.Brightness)
		}
		switch {
		case req.On != nil:
			z.on = *req.On
		case req.Color != nil || req.Brightness != nil:
			z.on = true // поменяли цвет или яркость — значит, зона нужна включённой
		}
	}
	if !a.r.owned {
		a.r.owned = true
		log.Print("кольцо: берём под управление")
	}
	a.applyRing(time.Now())
	a.dirty = true
	return nil
}

func (a *Agent) ReleaseRing() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseRing()
}

func (a *Agent) releaseRing() {
	r := &a.r
	if !r.owned {
		return
	}
	r.owned = false
	if r.active {
		// В покое прошивка держит кольцо погашенным (idle.led). Последний чужой
		// кадр может оказаться оборванной анимацией, поэтому возвращаем его, только
		// если прошивка сейчас не в покое.
		fr := hw.RingFrame{}
		if !a.anim.RingIdle() {
			fr = r.firmware
		}
		if err := a.ring.Write(fr); err != nil {
			log.Printf("кольцо: запись: %v", err)
		}
		r.actual, r.active = fr, false
	}
	a.dirty = true
	log.Print("кольцо: отдаём прошивке")
}

// ---- часы ----

func (a *Agent) syncClock(now time.Time) {
	actual, err := a.clock.Read()
	if err != nil {
		log.Printf("часы: чтение: %v", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := &a.c
	if actual != c.actual {
		c.actual = actual
		a.dirty = true
		if !c.active || actual != c.written {
			c.changed, c.firmware = now, actual
		}
	}
	if !c.owned {
		return
	}
	if c.active && actual != c.written {
		c.active = false // прошивка перерисовала время или играет анимацию
		a.dirty = true
	}
	if !c.active {
		a.applyClock(now)
	}
}

func (a *Agent) applyClock(now time.Time) {
	c := &a.c
	if !c.active && !(a.anim.ClockIdle() && a.settled(c.changed, now, clockSettle)) {
		return
	}
	fr := c.frame()
	if err := a.clock.Write(fr, c.actual); err != nil {
		log.Printf("часы: запись: %v", err)
		return
	}
	c.written, c.actual, c.active = fr, fr, true
	a.dirty = true
}

func (a *Agent) SetClock(req api.ClockRequest) error {
	var text [hw.ClockSegmentCount]bool
	if req.Text != nil {
		t, err := hw.RenderText(*req.Text)
		if err != nil {
			return err
		}
		text = t
	}
	for name := range req.Segments {
		if _, ok := hw.SegmentIndex(name); !ok {
			return fmt.Errorf("нет сегмента %q", name)
		}
	}
	if req.Brightness != nil && (*req.Brightness < 0 || *req.Brightness > 255) {
		return fmt.Errorf("яркость %d: нужно 0..255", *req.Brightness)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	c := &a.c
	if !c.owned {
		// Начинаем с того, что показывала прошивка: клик по сегменту поменяет только его.
		c.owned, c.bright = true, 0
		for i, v := range c.firmware {
			c.segs[i] = v > 0
			c.bright = max(c.bright, v)
		}
		if c.bright == 0 {
			c.bright = defaultClockBrightness
		}
		log.Print("часы: берём под управление")
	}
	if req.Clear {
		c.segs = [hw.ClockSegmentCount]bool{}
	}
	if req.Text != nil {
		c.segs = text
	}
	for name, on := range req.Segments {
		i, _ := hw.SegmentIndex(name)
		c.segs[i] = on
	}
	if req.Brightness != nil {
		c.bright = uint8(*req.Brightness)
	}
	a.applyClock(time.Now())
	a.dirty = true
	return nil
}

func (a *Agent) ReleaseClock() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseClock()
}

func (a *Agent) releaseClock() {
	c := &a.c
	if !c.owned {
		return
	}
	c.owned = false
	if c.active {
		// Время могло устареть, но прошивка перерисует его на смене минуты.
		if err := a.clock.Write(c.firmware, c.actual); err != nil {
			log.Printf("часы: запись: %v", err)
		}
		c.actual, c.active = c.firmware, false
	}
	a.dirty = true
	log.Print("часы: отдаём прошивке")
}

// ---- состояние для API ----

func (a *Agent) State() api.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state()
}

func (a *Agent) state() api.State {
	st := api.State{
		Ring: api.Ring{Owned: a.r.owned, Active: a.r.active},
		Clock: api.Clock{
			Owned: a.c.owned, Active: a.c.active, Brightness: int(a.c.bright),
			Segments: map[string]bool{}, Actual: map[string]int{},
		},
		Firmware: api.Firmware{
			Ring: a.anim.Ring, Clock: a.anim.Clock,
			RingIdle: a.anim.RingIdle(), ClockIdle: a.anim.ClockIdle(),
		},
	}
	for i, z := range a.r.zones {
		st.Ring.Zones = append(st.Ring.Zones, api.Zone{Color: z.color.Hex(), Brightness: int(z.bright), On: z.on})
		st.Ring.Actual = append(st.Ring.Actual, a.r.actual[i].Hex())
	}
	for i, name := range hw.ClockSegments {
		st.Clock.Segments[name] = a.c.segs[i]
		st.Clock.Actual[name] = int(a.c.actual[i])
	}
	return st
}

func (a *Agent) subscribe() chan []byte {
	ch := make(chan []byte, 1)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	return ch
}

func (a *Agent) unsubscribe(ch chan []byte) {
	a.mu.Lock()
	delete(a.subs, ch)
	a.mu.Unlock()
}

// publish рассылает свежее состояние подписчикам не чаще раза в 100 мс.
func (a *Agent) publish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.dirty {
		return
	}
	a.dirty = false
	msg, err := json.Marshal(a.state())
	if err != nil {
		log.Printf("состояние: %v", err)
		return
	}
	for ch := range a.subs {
		select {
		case <-ch: // прошлое не успели забрать — заменяем свежим
		default:
		}
		ch <- msg
	}
}
