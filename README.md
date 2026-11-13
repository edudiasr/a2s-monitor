# a2s-monitor

Monitor de servidores de jogo em Go. Consulta cada servidor pelo protocolo de consulta da Steam
(A2S, o mesmo que a lista de servidores do jogo usa), guarda o histórico de jogadores online,
expõe métricas pro Prometheus e uma API JSON, e avisa no Discord quando um servidor cai ou volta.

Funciona com qualquer jogo no Steamworks: DayZ, Rust, ARK, CS, Valheim etc. Sem dependências
fora da biblioteca padrão do Go.

```
$ a2s-monitor query -players 127.0.0.1:27016
nome       DIA-Z | Hardcore
mapa       chernarusplus
jogadores  12 / 60
versão     1.28.160049
sistema    windows, dedicated
senha      false
VAC        true
resposta   1ms

12 jogador(es):
  Lobo1                         0 pts  1m0s
  Corvo1                        0 pts  2m0s
  ...
```

## O protocolo

O A2S é UDP: um pacote de pedido, um (ou vários) de resposta. O cliente em `internal/a2s` cobre:

- **A2S_INFO** com todos os campos, inclusive os opcionais do *Extra Data Flag* (porta, SteamID do
  servidor, keywords, GameID). Detalhe: o campo de ID do jogo tem 16 bits e o DayZ é o app 221100,
  então o ID certo só vem no GameID de 64 bits.
- **Challenge**: servidores atualizados respondem o primeiro pedido com um número de 4 bytes em vez
  dos dados, e só respondem de verdade quando o pedido volta com esse número. Existe pra impedir
  que alguém use o servidor pra amplificar ataque com IP falsificado.
- **Respostas divididas** em vários pacotes, que podem chegar fora de ordem; o cliente remonta pela
  numeração. A variante comprimida com bzip2 (só jogos antigos da engine Source) devolve erro claro.
- **A2S_PLAYER**: nome, pontuação e tempo conectado de cada jogador.

O parser nunca entra em pânico com resposta mal formada: o leitor guarda o primeiro erro e as
leituras seguintes viram no-op. Isso é verificado com **fuzzing** (`go test -fuzz`), que roda 30 s
no CI. Localmente, 20 s foram 487 mil entradas aleatórias sem nenhum pânico.

## Como decide que caiu

- Uma resposta já basta pra considerar **no ar**.
- Pra considerar **fora do ar** precisa de `fail_threshold` falhas seguidas (padrão 3). UDP perde
  pacote, e um pacote perdido não pode acordar ninguém de madrugada.
- Avisa uma vez ao cair e uma vez ao voltar, com quanto tempo ficou fora. O "fora desde" é a
  primeira falha da sequência, não a que confirmou a queda.
- Cada servidor é consultado na sua própria goroutine: um servidor que não responde e fica
  esperando o timeout não atrasa a consulta dos outros (tem teste pra isso).

```
level=WARN msg="servidor fora do ar" servidor=Teste erro="read udp ...: connection refused"
level=INFO msg="servidor voltou" servidor=Teste ficou_fora=4m30s
```

No Discord: `🔴 **Teste** parou de responder` e `🟢 **Teste** voltou (ficou fora por 4 min)`. As
menções ficam desligadas no webhook, então um servidor chamado `@everyone` não marca o canal inteiro.

## Endpoints

| Rota | O que devolve |
|---|---|
| `GET /metrics` | formato de texto do Prometheus |
| `GET /api/status` | estado de todos os servidores (JSON) |
| `GET /api/history/{nome}` | amostras de jogadores online, da mais antiga pra mais nova |
| `GET /healthz` | `ok` |

```
$ curl -s localhost:9105/metrics | grep -v "^#"
a2s_up{server="DIA-Z",address="127.0.0.1:27016"} 1
a2s_up{server="Teste",address="127.0.0.1:27099"} 0
a2s_players{server="DIA-Z",address="127.0.0.1:27016"} 12
a2s_max_players{server="DIA-Z",address="127.0.0.1:27016"} 60
a2s_latency_seconds{server="DIA-Z",address="127.0.0.1:27016"} 0.00186
a2s_checks_total{server="DIA-Z",address="127.0.0.1:27016"} 4
a2s_check_failures_total{server="Teste",address="127.0.0.1:27099"} 4
...
```

As métricas são escritas à mão: são seis, e o formato de texto do Prometheus é simples. Não
compensava puxar a biblioteca cliente e as dependências dela por isso. Os valores de label são
escapados (aspas, barra e quebra de linha), senão um nome de servidor com aspas quebraria a saída.

O histórico fica num buffer circular de tamanho fixo (`history_size`, padrão 24 h com intervalo de
30 s), então a memória não cresce com o tempo.

## Rodando

```bash
go build -o a2s-monitor ./cmd/a2s-monitor
cp servers.example.json servers.json      # coloque o IP e a porta de query dos seus servidores
./a2s-monitor run -config servers.json -listen :9105
```

No DayZ a porta de query é a `steamQueryPort` do `serverDZ.cfg` (27016 por padrão).

Sem servidor de jogo à mão, dá pra testar com o servidor falso embutido, que anuncia um servidor
de DayZ e varia o número de jogadores a cada 10 s:

```bash
./a2s-monitor fake -listen 127.0.0.1:27016 -name "Servidor de teste"
```

Com Docker (imagem final de 16 MB, só o binário estático, usuário sem privilégio):

```bash
docker build -t a2s-monitor .
docker run -p 9105:9105 -v $PWD/servers.json:/config/servers.json:ro a2s-monitor
```

O arquivo de configuração é lido com `DisallowUnknownFields`: um campo escrito errado (`"adress"`)
dá erro na hora em vez de ser ignorado em silêncio.

## Testes

```bash
go test -race ./...
go test -run='^$' -fuzz=FuzzParseInfo -fuzztime=30s ./internal/a2s
```

- cliente contra um servidor A2S falso de verdade, por UDP: com e sem challenge, resposta dividida
  em pacotes que chegam em ordem inversa, lista de jogadores, servidor mudo (timeout) e cinco tipos
  de resposta inválida
- regras de queda e volta com relógio controlado: uma falha não alarma, três alarmam uma vez só,
  a volta informa o tempo fora
- servidor lento não atrasa o rápido (com o `Run` de verdade e timers reais)
- formato das métricas, endpoints HTTP, webhook do Discord (inclusive quando ele responde 429)
- validação da configuração
- ponta a ponta: monitor + cliente + servidor falso por UDP, que depois fica mudo e dispara o aviso

## Estrutura

```
cmd/a2s-monitor/     run, query e fake
internal/a2s/        protocolo: leitura e escrita dos pacotes, cliente, servidor falso
internal/monitor/    configuração, estado e histórico, avisos, /metrics e /api
```

## Licença

MIT
