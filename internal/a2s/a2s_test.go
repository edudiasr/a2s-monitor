package a2s

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func dayz() Info {
	return Info{
		Protocol: 17, Name: "DIA-Z | Hardcore", Map: "chernarusplus", Folder: "dayz", Game: "DayZ",
		Players: 3, MaxPlayers: 60, ServerType: "dedicated", Environment: "windows",
		VAC: true, Version: "1.28.160049", Port: 2302, SteamID: 90123456789012345,
		Keywords: "battleye,no3rd,privHive", GameID: 221100,
	}
}

func start(t *testing.T, s *FakeServer) string {
	t.Helper()
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return addr
}

func TestInfoWithoutChallenge(t *testing.T) {
	want := dayz()
	addr := start(t, NewFakeServer(want, nil))

	got, rtt, err := (&Client{}).Info(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if *got != want {
		t.Fatalf("info diferente\n got %+v\nwant %+v", *got, want)
	}
	if rtt <= 0 {
		t.Fatal("tempo de resposta não medido")
	}
}

func TestInfoAnswersTheChallenge(t *testing.T) {
	srv := NewFakeServer(dayz(), nil)
	srv.RequireChallenge = true
	addr := start(t, srv)

	got, _, err := (&Client{}).Info(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "DIA-Z | Hardcore" {
		t.Fatalf("nome %q", got.Name)
	}
	if n := srv.Requests.Load(); n != 2 {
		t.Fatalf("esperava 2 pedidos (sem e com challenge), foram %d", n)
	}
}

func TestSplitResponseIsReassembledEvenOutOfOrder(t *testing.T) {
	info := dayz()
	info.Keywords = strings.Repeat("palavra,", 40) // resposta grande o bastante pra dividir
	srv := NewFakeServer(info, nil)
	srv.SplitSize = 64
	addr := start(t, srv)

	got, _, err := (&Client{}).Info(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Keywords != info.Keywords {
		t.Fatalf("keywords remontadas erradas: %q", got.Keywords)
	}
}

func TestPlayers(t *testing.T) {
	want := []Player{{"Lobo", 12, 3600.5}, {"Corvo", 0, 42}, {"Jogador com nome bem comprido ©", -3, 1}}
	addr := start(t, NewFakeServer(dayz(), want))

	got, err := (&Client{}).Players(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d jogadores, esperava %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("jogador %d: %+v != %+v", i, got[i], want[i])
		}
	}
}

func TestSilentServerTimesOut(t *testing.T) {
	srv := NewFakeServer(dayz(), nil)
	srv.Silent.Store(true)
	addr := start(t, srv)

	started := time.Now()
	_, _, err := (&Client{Timeout: 200 * time.Millisecond}).Info(context.Background(), addr)
	if err == nil || !strings.Contains(err.Error(), "não respondeu") {
		t.Fatalf("esperava timeout, veio %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout de 200 ms demorou %s", elapsed)
	}
}

// servidor de mentira que responde sempre o mesmo pacote cru
func rawServer(t *testing.T, reply []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			_, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(reply, from)
		}
	}()
	return conn.LocalAddr().String()
}

func TestMalformedResponsesAreErrorsNotPanics(t *testing.T) {
	full := encodeInfo(&Info{Name: "x", ServerType: "dedicated", Environment: "linux"})
	cases := map[string][]byte{
		"cortado no meio":    full[:20],
		"string sem fim":     append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'I', 17}, []byte("sem terminador")...),
		"tipo errado":        {0xFF, 0xFF, 0xFF, 0xFF, 'Z', 0, 0, 0},
		"cabeçalho estranho": {0x12, 0x34, 0x56, 0x78, 'I'},
		"comprimido (bzip2)": {0xFE, 0xFF, 0xFF, 0xFF, 0x01, 0x00, 0x00, 0x80, 2, 0, 0xE0, 0x04, 0},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			addr := rawServer(t, reply)
			_, _, err := (&Client{Timeout: 300 * time.Millisecond}).Info(context.Background(), addr)
			if err == nil {
				t.Fatal("aceitou resposta inválida")
			}
			if name == "comprimido (bzip2)" && !errors.Is(err, ErrCompressed) {
				t.Fatalf("esperava ErrCompressed, veio %v", err)
			}
		})
	}
}

func TestOptionalFieldsAreOptional(t *testing.T) {
	// resposta sem o byte de Extra Data Flag, como servidores antigos mandam
	full := encodeInfo(&Info{Name: "antigo", Version: "1.0", ServerType: "dedicated", Environment: "linux"})
	cut := strings.Index(string(full), "1.0\x00") + len("1.0\x00")
	info, err := parseInfo(full[4:cut])
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "antigo" || info.Port != 0 || info.Keywords != "" {
		t.Fatalf("%+v", info)
	}
}

func FuzzParseInfo(f *testing.F) {
	info := dayz()
	f.Add(encodeInfo(&info)[4:])
	f.Add([]byte{'I'})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseInfo(data) // não pode entrar em pânico com nenhuma entrada
	})
}
