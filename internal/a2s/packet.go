// Package a2s implementa as consultas A2S_INFO e A2S_PLAYER do protocolo de servidores da Steam
// (https://developer.valvesoftware.com/wiki/Server_queries), usadas por DayZ, Rust, CS e
// qualquer jogo no Steamworks.
package a2s

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	headerSingle = -1 // FF FF FF FF: resposta num pacote só
	headerSplit  = -2 // FE FF FF FF: resposta dividida em vários pacotes

	typeInfoRequest   = 'T'
	typeInfoResponse  = 'I'
	typePlayerRequest = 'U'
	typePlayerReply   = 'D'
	typeChallenge     = 'A'
)

var infoPayload = []byte("Source Engine Query\x00")

// ErrMalformed é devolvido quando a resposta não segue o formato do protocolo.
var ErrMalformed = errors.New("a2s: resposta mal formada")

// Info é a resposta do A2S_INFO.
type Info struct {
	Protocol    byte   `json:"protocol"`
	Name        string `json:"name"`
	Map         string `json:"map"`
	Folder      string `json:"folder"`
	Game        string `json:"game"`
	AppID       uint16 `json:"app_id"` // 16 bits: jogos com id maior (DayZ é 221100) mandam o certo em GameID
	Players     int    `json:"players"`
	MaxPlayers  int    `json:"max_players"`
	Bots        int    `json:"bots"`
	ServerType  string `json:"server_type"` // dedicated, listen, proxy
	Environment string `json:"environment"` // linux, windows, mac
	Password    bool   `json:"password"`
	VAC         bool   `json:"vac"`
	Version     string `json:"version"`
	Port        uint16 `json:"port,omitempty"`
	SteamID     uint64 `json:"steam_id,omitempty"`
	Keywords    string `json:"keywords,omitempty"`
	GameID      uint64 `json:"game_id,omitempty"`
}

// Player é uma linha da resposta do A2S_PLAYER.
type Player struct {
	Name     string  `json:"name"`
	Score    int32   `json:"score"`
	Duration float32 `json:"duration_seconds"`
}

// reader lê os tipos do protocolo (tudo little-endian, strings terminadas em \0).
// O primeiro erro fica guardado e as leituras seguintes viram no-op, então o parser
// lê tudo em sequência e confere o erro uma vez só no fim.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return make([]byte, n)
	}
	if len(r.b) < n {
		r.err = fmt.Errorf("%w: faltam %d bytes", ErrMalformed, n-len(r.b))
		r.b = nil
		return make([]byte, n)
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u8() byte       { return r.take(1)[0] }
func (r *reader) u16() uint16    { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *reader) i32() int32     { return int32(binary.LittleEndian.Uint32(r.take(4))) }
func (r *reader) u64() uint64    { return binary.LittleEndian.Uint64(r.take(8)) }
func (r *reader) f32() float32   { return math.Float32frombits(binary.LittleEndian.Uint32(r.take(4))) }
func (r *reader) remaining() int { return len(r.b) }
func (r *reader) string() string {
	if r.err != nil {
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.err = fmt.Errorf("%w: string sem terminador", ErrMalformed)
	r.b = nil
	return ""
}

func parseInfo(payload []byte) (*Info, error) {
	r := &reader{b: payload}
	if t := r.u8(); r.err == nil && t != typeInfoResponse {
		return nil, fmt.Errorf("%w: esperava 'I', veio %q", ErrMalformed, t)
	}
	info := &Info{
		Protocol: r.u8(),
		Name:     r.string(),
		Map:      r.string(),
		Folder:   r.string(),
		Game:     r.string(),
		AppID:    r.u16(),
	}
	info.Players = int(r.u8())
	info.MaxPlayers = int(r.u8())
	info.Bots = int(r.u8())
	info.ServerType = map[byte]string{'d': "dedicated", 'l': "listen", 'p': "proxy"}[r.u8()]
	info.Environment = map[byte]string{'l': "linux", 'w': "windows", 'm': "mac", 'o': "mac"}[r.u8()]
	info.Password = r.u8() == 1
	info.VAC = r.u8() == 1
	info.Version = r.string()
	if r.err != nil {
		return nil, r.err
	}

	// Extra Data Flag: campos opcionais, cada bit diz se o campo veio
	if r.remaining() > 0 {
		edf := r.u8()
		if edf&0x80 != 0 {
			info.Port = r.u16()
		}
		if edf&0x10 != 0 {
			info.SteamID = r.u64()
		}
		if edf&0x40 != 0 { // SourceTV: porta e nome, que o monitor não usa
			r.u16()
			r.string()
		}
		if edf&0x20 != 0 {
			info.Keywords = r.string()
		}
		if edf&0x01 != 0 {
			info.GameID = r.u64()
		}
	}
	return info, r.err
}

func parsePlayers(payload []byte) ([]Player, error) {
	r := &reader{b: payload}
	if t := r.u8(); r.err == nil && t != typePlayerReply {
		return nil, fmt.Errorf("%w: esperava 'D', veio %q", ErrMalformed, t)
	}
	count := int(r.u8())
	players := make([]Player, 0, count)
	for i := 0; i < count && r.err == nil; i++ {
		r.u8() // índice, sempre 0 na prática
		players = append(players, Player{Name: r.string(), Score: r.i32(), Duration: r.f32()})
	}
	return players, r.err
}

// writer monta pacotes; usado pelo servidor falso dos testes e do modo "fake".
type writer struct{ b []byte }

func (w *writer) u8(v byte)       { w.b = append(w.b, v) }
func (w *writer) u16(v uint16)    { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) i32(v int32)     { w.b = binary.LittleEndian.AppendUint32(w.b, uint32(v)) }
func (w *writer) u64(v uint64)    { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *writer) f32(v float32)   { w.b = binary.LittleEndian.AppendUint32(w.b, math.Float32bits(v)) }
func (w *writer) string(s string) { w.b = append(append(w.b, s...), 0) }

func encodeInfo(info *Info) []byte {
	w := &writer{}
	w.i32(headerSingle)
	w.u8(typeInfoResponse)
	w.u8(info.Protocol)
	w.string(info.Name)
	w.string(info.Map)
	w.string(info.Folder)
	w.string(info.Game)
	w.u16(info.AppID)
	w.u8(byte(info.Players))
	w.u8(byte(info.MaxPlayers))
	w.u8(byte(info.Bots))
	w.u8(map[string]byte{"dedicated": 'd', "listen": 'l', "proxy": 'p'}[info.ServerType])
	w.u8(map[string]byte{"linux": 'l', "windows": 'w', "mac": 'm'}[info.Environment])
	w.u8(boolByte(info.Password))
	w.u8(boolByte(info.VAC))
	w.string(info.Version)
	var edf byte = 0x80 | 0x10 | 0x20 | 0x01
	w.u8(edf)
	w.u16(info.Port)
	w.u64(info.SteamID)
	w.string(info.Keywords)
	w.u64(info.GameID)
	return w.b
}

func encodePlayers(players []Player) []byte {
	w := &writer{}
	w.i32(headerSingle)
	w.u8(typePlayerReply)
	w.u8(byte(len(players)))
	for _, p := range players {
		w.u8(0)
		w.string(p.Name)
		w.i32(p.Score)
		w.f32(p.Duration)
	}
	return w.b
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
