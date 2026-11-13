package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edudiasr/a2s-monitor/internal/a2s"
)

// querier falso: cada servidor responde o que o teste mandar
type fakeQuerier struct {
	mu      sync.Mutex
	down    map[string]bool
	players map[string]int
	delay   map[string]time.Duration
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{down: map[string]bool{}, players: map[string]int{}, delay: map[string]time.Duration{}}
}

func (f *fakeQuerier) set(addr string, down bool, players int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[addr], f.players[addr] = down, players
}

func (f *fakeQuerier) Info(ctx context.Context, addr string) (*a2s.Info, time.Duration, error) {
	f.mu.Lock()
	down, players, delay := f.down[addr], f.players[addr], f.delay[addr]
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, 0, errors.New("timeout")
		}
	}
	if down {
		return nil, 0, errors.New("não respondeu")
	}
	return &a2s.Info{Name: "srv " + addr, Map: "chernarusplus", Players: players, MaxPlayers: 60}, 12 * time.Millisecond, nil
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Notify(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTestMonitor(q Querier, n Notifier) (*Monitor, *time.Time) {
	cfg := Config{
		Servers:       []Target{{"Principal", "a:1"}, {"Teste", "b:2"}},
		Interval:      Duration{30 * time.Second},
		Timeout:       Duration{time.Second},
		FailThreshold: 3,
		HistorySize:   5,
	}
	m := New(cfg, q, n, quiet)
	now := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return m, &now
}

func status(m *Monitor, name string) Status {
	for _, s := range m.Statuses() {
		if s.Name == name {
			return s
		}
	}
	panic(name)
}

func TestOneLostPacketDoesNotRaiseAnAlarm(t *testing.T) {
	q, rec := newFakeQuerier(), &recorder{}
	m, now := newTestMonitor(q, rec)
	target := Target{"Principal", "a:1"}

	q.set("a:1", false, 10)
	m.Check(context.Background(), target)
	q.set("a:1", true, 0)
	for i := 0; i < 2; i++ { // duas falhas: abaixo do limite
		*now = now.Add(30 * time.Second)
		m.Check(context.Background(), target)
	}
	q.set("a:1", false, 11)
	*now = now.Add(30 * time.Second)
	m.Check(context.Background(), target)

	if ev := rec.all(); len(ev) != 0 {
		t.Fatalf("não devia avisar nada, avisou %+v", ev)
	}
	s := status(m, "Principal")
	if !s.Up || s.ConsecutiveFailures != 0 || s.Failures != 2 || s.Checks != 4 {
		t.Fatalf("%+v", s)
	}
}

func TestDownAfterThresholdAndBackWithDowntime(t *testing.T) {
	q, rec := newFakeQuerier(), &recorder{}
	m, now := newTestMonitor(q, rec)
	target := Target{"Principal", "a:1"}
	start := *now

	q.set("a:1", false, 10)
	m.Check(context.Background(), target)

	q.set("a:1", true, 0)
	for i := 1; i <= 3; i++ {
		*now = start.Add(time.Duration(i) * 30 * time.Second)
		m.Check(context.Background(), target)
	}
	events := rec.all()
	if len(events) != 1 || events[0].Up {
		t.Fatalf("esperava 1 aviso de queda, veio %+v", events)
	}
	s := status(m, "Principal")
	if s.Up || s.Players != 0 {
		t.Fatalf("%+v", s)
	}
	// "desde" é a primeira falha (30 s), não a terceira (90 s)
	if want := start.Add(30 * time.Second); !s.Since.Equal(want) {
		t.Fatalf("since %v, esperava %v", s.Since, want)
	}

	// continua fora: não repete o aviso
	*now = start.Add(4 * 30 * time.Second)
	m.Check(context.Background(), target)
	if len(rec.all()) != 1 {
		t.Fatal("repetiu o aviso de queda")
	}

	q.set("a:1", false, 8)
	*now = start.Add(10 * time.Minute)
	m.Check(context.Background(), target)
	events = rec.all()
	if len(events) != 2 || !events[1].Up {
		t.Fatalf("esperava aviso de volta, veio %+v", events)
	}
	if want := 10*time.Minute - 30*time.Second; events[1].Downtime != want {
		t.Fatalf("ficou fora %s, esperava %s", events[1].Downtime, want)
	}
	if !strings.Contains(events[1].Message(), "9 min") {
		t.Fatalf("mensagem: %s", events[1].Message())
	}
}

func TestServerThatNeverAnsweredIsReportedAfterThreshold(t *testing.T) {
	q, rec := newFakeQuerier(), &recorder{}
	m, _ := newTestMonitor(q, rec)
	q.set("b:2", true, 0)
	for i := 0; i < 3; i++ {
		m.Check(context.Background(), Target{"Teste", "b:2"})
	}
	if s := status(m, "Teste"); !s.Known || s.Up {
		t.Fatalf("%+v", s)
	}
	if len(rec.all()) != 1 {
		t.Fatal("servidor que já sobe fora do ar também precisa avisar")
	}
}

func TestHistoryKeepsOnlyTheLastSamples(t *testing.T) {
	q := newFakeQuerier()
	m, now := newTestMonitor(q, NoNotifier{})
	for i := 1; i <= 7; i++ {
		q.set("a:1", false, i)
		*now = now.Add(30 * time.Second)
		m.Check(context.Background(), Target{"Principal", "a:1"})
	}
	samples, ok := m.History("Principal")
	if !ok || len(samples) != 5 {
		t.Fatalf("%d amostras", len(samples))
	}
	for i, s := range samples {
		if s.Players != i+3 {
			t.Fatalf("amostra %d com %d jogadores (ordem errada?)", i, s.Players)
		}
	}
	if _, ok := m.History("nao-existe"); ok {
		t.Fatal("histórico de servidor inexistente")
	}
}

func TestSlowServerDoesNotDelayTheOthers(t *testing.T) {
	q := newFakeQuerier()
	q.set("a:1", false, 5)
	q.set("b:2", false, 7)
	q.delay["b:2"] = 800 * time.Millisecond
	cfg := Config{
		Servers:  []Target{{"Principal", "a:1"}, {"Teste", "b:2"}},
		Interval: Duration{50 * time.Millisecond}, Timeout: Duration{40 * time.Millisecond},
		FailThreshold: 2, HistorySize: 100,
	}
	m := New(cfg, q, NoNotifier{}, quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	m.Run(ctx)

	fast, slow := status(m, "Principal"), status(m, "Teste")
	if fast.Checks < 8 {
		t.Fatalf("o servidor rápido só foi consultado %d vezes", fast.Checks)
	}
	if slow.Up {
		t.Fatalf("o lento estoura o timeout e devia estar fora: %+v", slow)
	}
}

func TestMetricsFormat(t *testing.T) {
	q := newFakeQuerier()
	m, _ := newTestMonitor(q, NoNotifier{})
	m.cfg.Servers[0].Name = "Principal"
	q.set("a:1", false, 23)
	m.Check(context.Background(), Target{"Principal", "a:1"})

	var b strings.Builder
	m.WriteMetrics(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE a2s_up gauge",
		`a2s_up{server="Principal",address="a:1"} 1`,
		`a2s_players{server="Principal",address="a:1"} 23`,
		`a2s_max_players{server="Principal",address="a:1"} 60`,
		`a2s_latency_seconds{server="Principal",address="a:1"} 0.012`,
		"# TYPE a2s_checks_total counter",
		`a2s_up{server="Teste",address="b:2"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("faltou %q em\n%s", want, out)
		}
	}
	if got := escapeLabel("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Errorf("escape: %s", got)
	}
}

func TestHTTPEndpoints(t *testing.T) {
	q := newFakeQuerier()
	m, _ := newTestMonitor(q, NoNotifier{})
	q.set("a:1", false, 4)
	m.Check(context.Background(), Target{"Principal", "a:1"})
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var statuses []Status
	_ = json.NewDecoder(resp.Body).Decode(&statuses)
	resp.Body.Close()
	if len(statuses) != 2 || statuses[0].Name != "Principal" || statuses[0].Players != 4 {
		t.Fatalf("%+v", statuses)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("sem CORS o dashboard não consegue ler")
	}

	resp, _ = http.Get(srv.URL + "/api/history/Principal")
	var samples []Sample
	_ = json.NewDecoder(resp.Body).Decode(&samples)
	resp.Body.Close()
	if len(samples) != 1 || samples[0].Players != 4 {
		t.Fatalf("%+v", samples)
	}

	resp, _ = http.Get(srv.URL + "/api/history/Ninguem")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}

	resp, _ = http.Post(srv.URL+"/api/status", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST devia ser recusado, veio %d", resp.StatusCode)
	}
}

func TestDiscordWebhook(t *testing.T) {
	var got map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	err := Discord{URL: hook.URL}.Notify(context.Background(), Event{Server: "@everyone", Address: "a:1", Error: "timeout"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got["content"].(string), "parou de responder") {
		t.Fatalf("%v", got)
	}
	if mentions := got["allowed_mentions"].(map[string]any)["parse"].([]any); len(mentions) != 0 {
		t.Fatal("menções precisam estar desligadas")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer failing.Close()
	if err := (Discord{URL: failing.URL}).Notify(context.Background(), Event{}); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("erro do webhook não foi devolvido: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "cfg.json")
		_ = os.WriteFile(p, []byte(body), 0o600)
		return p
	}

	cfg, err := LoadConfig(write(`{"servers":[{"name":"A","address":"1.2.3.4:27016"}],"interval":"1m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval.Duration != time.Minute || cfg.Timeout.Duration != 3*time.Second || cfg.FailThreshold != 3 {
		t.Fatalf("%+v", cfg)
	}

	for body, want := range map[string]string{
		`{"servers":[]}`: "nenhum servidor",
		`{"servers":[{"name":"A","address":"x:1"},{"name":"A","address":"y:1"}]}`:   "repetido",
		`{"servers":[{"name":"A","address":"x:1"}],"interval":"2s","timeout":"5s"}`: "menor que interval",
		`{"servers":[{"name":"A","adress":"x:1"}]}`:                                 "unknown field",
		`{"servers":[{"name":"A","address":"x:1"}],"interval":30}`:                  "texto",
	} {
		if _, err := LoadConfig(write(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: esperava erro com %q, veio %v", body, want, err)
		}
	}
}

// ponta a ponta: monitor de verdade, cliente de verdade, servidor A2S falso por UDP
func TestEndToEndWithRealUDP(t *testing.T) {
	srv := a2s.NewFakeServer(a2s.Info{Name: "Real", Map: "enoch", MaxPlayers: 40, ServerType: "dedicated", Environment: "linux"},
		[]a2s.Player{{Name: "Lobo"}, {Name: "Corvo"}})
	srv.RequireChallenge = true
	srv.Update(func(*a2s.Info, *[]a2s.Player) {})
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cfg := Config{Servers: []Target{{"Real", addr}}, Interval: Duration{time.Second}, Timeout: Duration{500 * time.Millisecond}}
	rec := &recorder{}
	m := New(cfg, &a2s.Client{Timeout: 500 * time.Millisecond}, rec, quiet)
	m.Check(context.Background(), cfg.Servers[0])
	if s := status(m, "Real"); !s.Up || s.Players != 2 || s.Map != "enoch" {
		t.Fatalf("%+v", s)
	}

	srv.Silent.Store(true)
	for i := 0; i < 3; i++ {
		m.Check(context.Background(), cfg.Servers[0])
	}
	if s := status(m, "Real"); s.Up || len(rec.all()) != 1 {
		t.Fatalf("%+v %+v", s, rec.all())
	}
}
