package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Duration aceita "30s", "2m" no JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duração precisa ser texto como \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

type Target struct {
	Name    string `json:"name"`
	Address string `json:"address"` // host:porta de query (no DayZ, a porta definida em steamQueryPort)
}

type Config struct {
	Servers       []Target `json:"servers"`
	Interval      Duration `json:"interval"`
	Timeout       Duration `json:"timeout"`
	FailThreshold int      `json:"fail_threshold"` // falhas seguidas pra considerar fora do ar
	WebhookURL    string   `json:"webhook_url"`    // Discord; vazio = só log
	HistorySize   int      `json:"history_size"`   // amostras guardadas por servidor
}

func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // campo escrito errado no arquivo vira erro, não é ignorado em silêncio
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg.withDefaults(), cfg.withDefaults().Validate()
}

func (c Config) withDefaults() Config {
	if c.Interval.Duration == 0 {
		c.Interval.Duration = 30 * time.Second
	}
	if c.Timeout.Duration == 0 {
		c.Timeout.Duration = 3 * time.Second
	}
	if c.FailThreshold == 0 {
		c.FailThreshold = 3
	}
	if c.HistorySize == 0 {
		c.HistorySize = 2880 // 24 h com intervalo de 30 s
	}
	return c
}

func (c Config) Validate() error {
	var errs []error
	if len(c.Servers) == 0 {
		errs = append(errs, errors.New("nenhum servidor em \"servers\""))
	}
	seen := map[string]bool{}
	for i, s := range c.Servers {
		if s.Name == "" || s.Address == "" {
			errs = append(errs, fmt.Errorf("servers[%d]: name e address são obrigatórios", i))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Errorf("servers[%d]: nome %q repetido", i, s.Name))
		}
		seen[s.Name] = true
	}
	if c.Timeout.Duration >= c.Interval.Duration {
		errs = append(errs, errors.New("timeout precisa ser menor que interval"))
	}
	return errors.Join(errs...)
}
