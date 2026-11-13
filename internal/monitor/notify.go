package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Event é uma mudança de estado: caiu ou voltou.
type Event struct {
	Server   string
	Address  string
	Up       bool
	At       time.Time
	Downtime time.Duration // só quando voltou
	Error    string        // só quando caiu
}

func (e Event) Message() string {
	if e.Up {
		return fmt.Sprintf("🟢 **%s** voltou (ficou fora por %s)", e.Server, humanDuration(e.Downtime))
	}
	return fmt.Sprintf("🔴 **%s** parou de responder (%s): %s", e.Server, e.Address, e.Error)
}

type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Discord manda o aviso pra um webhook de canal do Discord.
type Discord struct {
	URL    string
	Client *http.Client
}

func (d Discord) Notify(ctx context.Context, e Event) error {
	body, _ := json.Marshal(map[string]any{
		"content": e.Message(),
		// sem isso um nome de servidor com @everyone marcaria o canal inteiro
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("webhook respondeu %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// NoNotifier é usado quando não há webhook configurado: a mudança só vai pro log.
type NoNotifier struct{}

func (NoNotifier) Notify(context.Context, Event) error { return nil }

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h %02d min", int(d.Hours()), int(d.Minutes())%60)
	}
}
