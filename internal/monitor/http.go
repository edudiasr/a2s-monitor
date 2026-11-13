package monitor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Handler expõe /metrics (Prometheus), /api/status, /api/history e /healthz.
func (m *Monitor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.WriteMetrics(w)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Statuses())
	})
	mux.HandleFunc("GET /api/history/{name}", func(w http.ResponseWriter, r *http.Request) {
		samples, ok := m.History(r.PathValue("name"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "servidor não encontrado"})
			return
		}
		writeJSON(w, http.StatusOK, samples)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return cors(mux)
}

// O estado dos servidores é público (é o que o próprio jogo mostra na lista de servidores),
// então qualquer origem pode ler. Só GET passa.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteMetrics escreve no formato de texto do Prometheus. Feito à mão: são cinco métricas
// e o formato é simples, não justifica puxar a biblioteca cliente inteira.
func (m *Monitor) WriteMetrics(w io.Writer) {
	statuses := m.Statuses()
	type metric struct {
		name, help, kind string
		value            func(Status) float64
	}
	metrics := []metric{
		{"a2s_up", "1 se o servidor está respondendo, 0 se foi considerado fora do ar.", "gauge",
			func(s Status) float64 { return boolFloat(s.Up) }},
		{"a2s_players", "Jogadores online na última consulta.", "gauge",
			func(s Status) float64 { return float64(s.Players) }},
		{"a2s_max_players", "Vagas do servidor.", "gauge",
			func(s Status) float64 { return float64(s.MaxPlayers) }},
		{"a2s_latency_seconds", "Tempo de resposta da última consulta que deu certo.", "gauge",
			func(s Status) float64 { return s.LatencyMs / 1000 }},
		{"a2s_checks_total", "Consultas feitas.", "counter",
			func(s Status) float64 { return float64(s.Checks) }},
		{"a2s_check_failures_total", "Consultas sem resposta ou com resposta inválida.", "counter",
			func(s Status) float64 { return float64(s.Failures) }},
	}
	for _, mt := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", mt.name, mt.help, mt.name, mt.kind)
		for _, s := range statuses {
			fmt.Fprintf(w, "%s{server=\"%s\",address=\"%s\"} %g\n", mt.name, escapeLabel(s.Name), escapeLabel(s.Address), mt.value(s))
		}
	}
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
