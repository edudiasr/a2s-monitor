// Package monitor consulta vários servidores em paralelo, guarda o estado e o histórico de cada um
// e avisa quando um cai ou volta.
package monitor

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/edudiasr/a2s-monitor/internal/a2s"
)

// Querier é o que o monitor precisa do cliente A2S (nos testes, uma versão falsa).
type Querier interface {
	Info(ctx context.Context, addr string) (*a2s.Info, time.Duration, error)
}

// Status é o estado atual de um servidor, como sai na API.
type Status struct {
	Name                string    `json:"name"`
	Address             string    `json:"address"`
	Up                  bool      `json:"up"`
	Known               bool      `json:"known"` // false até a primeira conclusão (ainda não respondeu nem estourou o limite de falhas)
	Since               time.Time `json:"since"` // desde quando está no estado atual
	LastCheck           time.Time `json:"last_check"`
	LatencyMs           float64   `json:"latency_ms"`
	ServerName          string    `json:"server_name,omitempty"`
	Map                 string    `json:"map,omitempty"`
	Players             int       `json:"players"`
	MaxPlayers          int       `json:"max_players"`
	Version             string    `json:"version,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
	Checks              uint64    `json:"checks"`
	Failures            uint64    `json:"failures"`
}

// Sample é um ponto do histórico.
type Sample struct {
	At      time.Time `json:"at"`
	Up      bool      `json:"up"`
	Players int       `json:"players"`
}

type serverState struct {
	status  Status
	history *ring
}

type Monitor struct {
	cfg      Config
	querier  Querier
	notifier Notifier
	now      func() time.Time
	log      *slog.Logger

	mu      sync.RWMutex
	servers map[string]*serverState
}

func New(cfg Config, q Querier, n Notifier, log *slog.Logger) *Monitor {
	cfg = cfg.withDefaults()
	m := &Monitor{cfg: cfg, querier: q, notifier: n, now: time.Now, log: log, servers: map[string]*serverState{}}
	for _, t := range cfg.Servers {
		m.servers[t.Name] = &serverState{
			status:  Status{Name: t.Name, Address: t.Address},
			history: newRing(cfg.HistorySize),
		}
	}
	return m
}

// Run consulta cada servidor no seu próprio ritmo até o contexto acabar. Um servidor lento
// (esperando o timeout) não atrasa a consulta dos outros.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range m.cfg.Servers {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			ticker := time.NewTicker(m.cfg.Interval.Duration)
			defer ticker.Stop()
			for {
				m.Check(ctx, t)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(t)
	}
	wg.Wait()
}

// Check faz uma consulta e registra o resultado.
func (m *Monitor) Check(ctx context.Context, t Target) {
	qctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout.Duration)
	info, rtt, err := m.querier.Info(qctx, t.Address)
	cancel()
	if ctx.Err() != nil {
		return // desligando: não conta como falha
	}
	if ev := m.record(t.Name, info, rtt, err); ev != nil {
		if ev.Up {
			m.log.Info("servidor voltou", "servidor", ev.Server, "ficou_fora", ev.Downtime.Round(time.Second))
		} else {
			m.log.Warn("servidor fora do ar", "servidor", ev.Server, "erro", ev.Error)
		}
		if err := m.notifier.Notify(ctx, *ev); err != nil {
			m.log.Warn("aviso não enviado", "servidor", ev.Server, "erro", err)
		}
	}
}

// record aplica o resultado de uma consulta e diz se houve mudança de estado que merece aviso.
//
// Regras:
//   - uma resposta já basta pra considerar no ar;
//   - pra considerar fora do ar precisa de FailThreshold falhas seguidas (um pacote UDP perdido
//     não pode disparar alarme);
//   - avisa ao cair, e ao voltar avisa junto quanto tempo ficou fora.
func (m *Monitor) record(name string, info *a2s.Info, rtt time.Duration, err error) *Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.servers[name]
	st := &s.status
	now := m.now()
	st.LastCheck = now
	st.Checks++

	var ev *Event
	if err == nil {
		wasDown := st.Known && !st.Up
		if !st.Up || !st.Known {
			if wasDown {
				ev = &Event{Server: name, Address: st.Address, Up: true, At: now, Downtime: now.Sub(st.Since)}
			}
			st.Up, st.Known, st.Since = true, true, now
		}
		st.ConsecutiveFailures = 0
		st.LastError = ""
		st.LatencyMs = float64(rtt.Microseconds()) / 1000
		st.ServerName, st.Map, st.Version = info.Name, info.Map, info.Version
		st.Players, st.MaxPlayers = info.Players, info.MaxPlayers
	} else {
		st.Failures++
		st.ConsecutiveFailures++
		st.LastError = err.Error()
		if st.ConsecutiveFailures >= m.cfg.FailThreshold && (st.Up || !st.Known) {
			// "since" é o momento da primeira falha da sequência, não o da que confirmou a queda
			firstFailure := now.Add(-time.Duration(st.ConsecutiveFailures-1) * m.cfg.Interval.Duration)
			st.Up, st.Known, st.Since = false, true, firstFailure
			st.Players = 0
			ev = &Event{Server: name, Address: st.Address, Up: false, At: now, Error: err.Error()}
		}
	}
	s.history.push(Sample{At: now, Up: st.Up && err == nil, Players: st.Players})
	return ev
}

// Statuses devolve o estado de todos, em ordem de nome.
func (m *Monitor) Statuses() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// History devolve as amostras de um servidor (da mais antiga pra mais nova) e se ele existe.
func (m *Monitor) History(name string) ([]Sample, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.servers[name]
	if !ok {
		return nil, false
	}
	return s.history.items(), true
}

// ring guarda as últimas N amostras sem crescer a memória.
type ring struct {
	buf  []Sample
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]Sample, n)} }

func (r *ring) push(s Sample) {
	r.buf[r.next] = s
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

func (r *ring) items() []Sample {
	if !r.full {
		return append([]Sample(nil), r.buf[:r.next]...)
	}
	return append(append([]Sample(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}
