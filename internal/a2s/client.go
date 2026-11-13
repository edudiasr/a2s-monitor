package a2s

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// ErrCompressed: resposta dividida e comprimida com bzip2. Só jogos antigos da engine Source
// fazem isso; DayZ e os atuais não.
var ErrCompressed = errors.New("a2s: resposta comprimida não suportada")

const maxPacket = 1400 // limite de payload que os servidores Source usam por pacote UDP

// Client consulta servidores. O zero value funciona, com timeout de 3 s.
type Client struct {
	Timeout time.Duration
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 3 * time.Second
}

// Info faz o A2S_INFO e devolve também o tempo de resposta (do primeiro envio até a resposta final,
// incluindo a volta do challenge quando o servidor pede).
func (c *Client) Info(ctx context.Context, addr string) (*Info, time.Duration, error) {
	started := time.Now()
	payload, err := c.query(ctx, addr, typeInfoRequest, infoPayload, nil)
	if err != nil {
		return nil, 0, err
	}
	info, err := parseInfo(payload)
	return info, time.Since(started), err
}

// Players faz o A2S_PLAYER. Esse sempre exige challenge: o primeiro pedido vai com FF FF FF FF.
func (c *Client) Players(ctx context.Context, addr string) ([]Player, error) {
	payload, err := c.query(ctx, addr, typePlayerRequest, nil, []byte{0xFF, 0xFF, 0xFF, 0xFF})
	if err != nil {
		return nil, err
	}
	return parsePlayers(payload)
}

func (c *Client) query(ctx context.Context, addr string, kind byte, body, challenge []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	// o servidor pode responder com um challenge em vez da resposta (proteção contra
	// amplificação de UDP com IP falsificado); aí o pedido é refeito com o número recebido
	for attempt := 0; attempt < 3; attempt++ {
		req := []byte{0xFF, 0xFF, 0xFF, 0xFF, kind}
		req = append(req, body...)
		req = append(req, challenge...)
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		payload, err := readResponse(conn)
		if err != nil {
			// o prazo da conexão e o do contexto vencem juntos, mas qualquer um dos dois pode
			// ser percebido primeiro: os dois casos são "não respondeu a tempo"
			if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, fmt.Errorf("a2s: %s não respondeu em %s", addr, c.timeout())
			}
			return nil, err
		}
		if len(payload) == 5 && payload[0] == typeChallenge {
			challenge = payload[1:5]
			continue
		}
		return payload, nil
	}
	return nil, fmt.Errorf("%w: challenge em loop", ErrMalformed)
}

// readResponse lê um pacote e, se for resposta dividida, junta as partes (que podem chegar
// fora de ordem). Devolve o payload sem o cabeçalho FF FF FF FF.
func readResponse(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 65535)
	var (
		id    int32
		total int
		parts map[int][]byte
	)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		if n < 5 {
			return nil, fmt.Errorf("%w: pacote de %d bytes", ErrMalformed, n)
		}
		header := int32(binary.LittleEndian.Uint32(buf[:4]))
		switch header {
		case headerSingle:
			return append([]byte(nil), buf[4:n]...), nil
		case headerSplit:
			if n < 12 {
				return nil, fmt.Errorf("%w: cabeçalho de pacote dividido curto", ErrMalformed)
			}
			packetID := int32(binary.LittleEndian.Uint32(buf[4:8]))
			if uint32(packetID)&0x80000000 != 0 {
				return nil, ErrCompressed
			}
			count, number := int(buf[8]), int(buf[9])
			// buf[10:12] é o tamanho máximo de pacote que o servidor usa; não precisa
			if parts == nil {
				id, total, parts = packetID, count, make(map[int][]byte, count)
			}
			if packetID != id || count != total || number >= total {
				return nil, fmt.Errorf("%w: partes de respostas diferentes misturadas", ErrMalformed)
			}
			parts[number] = append([]byte(nil), buf[12:n]...)
			if len(parts) < total {
				continue
			}
			var whole []byte
			for i := 0; i < total; i++ {
				whole = append(whole, parts[i]...)
			}
			if len(whole) < 4 || int32(binary.LittleEndian.Uint32(whole[:4])) != headerSingle {
				return nil, fmt.Errorf("%w: resposta remontada sem cabeçalho", ErrMalformed)
			}
			return whole[4:], nil
		default:
			return nil, fmt.Errorf("%w: cabeçalho %#x", ErrMalformed, uint32(header))
		}
	}
}
