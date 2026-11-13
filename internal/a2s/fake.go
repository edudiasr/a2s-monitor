package a2s

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
)

// FakeServer responde A2S como um servidor de jogo. Serve pros testes e pro modo
// "a2s-monitor fake", que deixa testar o monitor e o dashboard sem um servidor de verdade.
type FakeServer struct {
	RequireChallenge bool // pede challenge também no A2S_INFO, como os servidores atualizados
	SplitSize        int  // > 0: divide respostas maiores que isso em vários pacotes
	Silent           atomic.Bool

	mu        sync.Mutex
	info      Info
	players   []Player
	challenge [4]byte
	conn      net.PacketConn
	Requests  atomic.Int64
}

func NewFakeServer(info Info, players []Player) *FakeServer {
	s := &FakeServer{info: info, players: players}
	_, _ = rand.Read(s.challenge[:])
	return s
}

// Update troca o estado que o servidor anuncia (ex.: jogadores entrando e saindo).
func (s *FakeServer) Update(fn func(info *Info, players *[]Player)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.info, &s.players)
	s.info.Players = len(s.players)
}

// Start abre a porta UDP e atende em segundo plano. Devolve o endereço real (útil com porta 0).
func (s *FakeServer) Start(addr string) (string, error) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return "", err
	}
	s.conn = conn
	go s.serve()
	return conn.LocalAddr().String(), nil
}

func (s *FakeServer) Close() error {
	if s.conn == nil {
		return nil
	}
	return s.conn.Close()
}

func (s *FakeServer) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		s.Requests.Add(1)
		if s.Silent.Load() || n < 5 || !bytes.Equal(buf[:4], []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
			continue
		}
		for _, packet := range s.reply(buf[4:n]) {
			_, _ = s.conn.WriteTo(packet, from)
		}
	}
}

func (s *FakeServer) reply(req []byte) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch req[0] {
	case typeInfoRequest:
		rest := req[1:]
		if !bytes.HasPrefix(rest, infoPayload) {
			return nil
		}
		if s.RequireChallenge && !bytes.Equal(rest[len(infoPayload):], s.challenge[:]) {
			return [][]byte{s.challengePacket()}
		}
		return s.split(encodeInfo(&s.info))
	case typePlayerRequest:
		if !bytes.Equal(req[1:], s.challenge[:]) {
			return [][]byte{s.challengePacket()}
		}
		return s.split(encodePlayers(s.players))
	}
	return nil
}

func (s *FakeServer) challengePacket() []byte {
	return append([]byte{0xFF, 0xFF, 0xFF, 0xFF, typeChallenge}, s.challenge[:]...)
}

// split divide no formato da engine Source; as partes saem em ordem inversa de propósito,
// pra garantir que o cliente remonta pela numeração e não pela ordem de chegada.
func (s *FakeServer) split(whole []byte) [][]byte {
	if s.SplitSize <= 0 || len(whole) <= s.SplitSize {
		return [][]byte{whole}
	}
	var chunks [][]byte
	for len(whole) > 0 {
		n := min(s.SplitSize, len(whole))
		chunks = append(chunks, whole[:n])
		whole = whole[n:]
	}
	packets := make([][]byte, len(chunks))
	for i, chunk := range chunks {
		h := make([]byte, 12, 12+len(chunk))
		binary.LittleEndian.PutUint32(h[0:4], uint32(0xFFFFFFFE))
		binary.LittleEndian.PutUint32(h[4:8], 0x1234)
		h[8], h[9] = byte(len(chunks)), byte(i)
		binary.LittleEndian.PutUint16(h[10:12], uint16(s.SplitSize))
		packets[len(chunks)-1-i] = append(h, chunk...)
	}
	return packets
}
