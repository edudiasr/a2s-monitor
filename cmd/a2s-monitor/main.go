// a2s-monitor: acompanha servidores de jogo pelo protocolo de consulta da Steam.
//
//	a2s-monitor run -config servers.json -listen :9105
//	a2s-monitor query 203.0.113.5:27016
//	a2s-monitor fake -listen 127.0.0.1:27016 -name "Servidor de teste"
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/edudiasr/a2s-monitor/internal/a2s"
	"github.com/edudiasr/a2s-monitor/internal/monitor"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = run(ctx, os.Args[2:])
	case "query":
		err = query(ctx, os.Args[2:])
	case "fake":
		err = fake(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `uso:
  a2s-monitor run   -config servers.json [-listen :9105]   monitora e serve /metrics e /api
  a2s-monitor query [-players] host:porta                  consulta um servidor uma vez
  a2s-monitor fake  [-listen 127.0.0.1:27016] [-name ...]  sobe um servidor falso pra testes
`)
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "servers.json", "arquivo de configuração")
	listen := fs.String("listen", ":9105", "endereço do HTTP (/metrics, /api/status)")
	_ = fs.Parse(args)

	cfg, err := monitor.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var notifier monitor.Notifier = monitor.NoNotifier{}
	if cfg.WebhookURL != "" {
		notifier = monitor.Discord{URL: cfg.WebhookURL}
	}
	m := monitor.New(cfg, &a2s.Client{Timeout: cfg.Timeout.Duration}, notifier, log)

	srv := &http.Server{Addr: *listen, Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("monitorando", "servidores", len(cfg.Servers), "intervalo", cfg.Interval.Duration, "http", *listen)

	go m.Run(ctx)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// desligamento educado: termina as respostas HTTP em andamento
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Info("desligando")
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func query(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	players := fs.Bool("players", false, "lista também os jogadores (A2S_PLAYER)")
	asJSON := fs.Bool("json", false, "saída em JSON")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("informe host:porta")
	}
	addr := fs.Arg(0)
	client := &a2s.Client{}

	info, rtt, err := client.Info(ctx, addr)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "nome\t%s\n", info.Name)
	fmt.Fprintf(w, "mapa\t%s\n", info.Map)
	fmt.Fprintf(w, "jogadores\t%d / %d\n", info.Players, info.MaxPlayers)
	fmt.Fprintf(w, "versão\t%s\n", info.Version)
	fmt.Fprintf(w, "sistema\t%s, %s\n", info.Environment, info.ServerType)
	fmt.Fprintf(w, "senha\t%v\n", info.Password)
	fmt.Fprintf(w, "VAC\t%v\n", info.VAC)
	fmt.Fprintf(w, "resposta\t%s\n", rtt.Round(time.Millisecond))
	_ = w.Flush()

	if *players {
		list, err := client.Players(ctx, addr)
		if err != nil {
			return err
		}
		fmt.Printf("\n%d jogador(es):\n", len(list))
		for _, p := range list {
			fmt.Printf("  %-24s %6d pts  %s\n", p.Name, p.Score, time.Duration(p.Duration)*time.Second)
		}
	}
	return nil
}

// fake sobe um servidor A2S que muda o número de jogadores aos poucos, pra demonstrar o
// monitor e o dashboard sem precisar de um servidor de jogo de verdade.
func fake(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fake", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:27016", "endereço UDP")
	name := fs.String("name", "Servidor de teste", "nome anunciado")
	mapName := fs.String("map", "chernarusplus", "mapa anunciado")
	maxPlayers := fs.Int("max", 60, "vagas")
	_ = fs.Parse(args)

	names := []string{"Lobo", "Corvo", "Raposa", "Coruja", "Tatu", "Capivara", "Jaguar", "Sabiá", "Lontra", "Quati"}
	srv := a2s.NewFakeServer(a2s.Info{
		Protocol: 17, Name: *name, Map: *mapName, Folder: "dayz", Game: "DayZ",
		MaxPlayers: *maxPlayers, ServerType: "dedicated", Environment: "windows", VAC: true,
		Version: "1.28.160049", Port: 2302, Keywords: "battleye,no3rd,privHive,etm3.000000,entm1.000000",
		GameID: 221100,
	}, nil)
	srv.RequireChallenge = true
	addr, err := srv.Start(*listen)
	if err != nil {
		return err
	}
	defer srv.Close()
	fmt.Fprintf(os.Stderr, "servidor falso em %s (udp)\n", addr)

	online := 12
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		srv.Update(func(_ *a2s.Info, players *[]a2s.Player) {
			*players = (*players)[:0]
			for i := range online {
				*players = append(*players, a2s.Player{Name: fmt.Sprintf("%s%d", names[i%len(names)], i/len(names)+1), Duration: float32(60 * (i + 1))})
			}
		})
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			online = max(0, min(*maxPlayers, online+rand.IntN(5)-2))
		}
	}
}
