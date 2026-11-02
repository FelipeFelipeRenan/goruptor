# Desempenho: análise de gargalos

Este documento registra **o que foi medido, o que foi corrigido e o que foi deixado como está**.
Todo número abaixo vem dos benchmarks do repositório (`make bench`).

> **Como ler os números.** Medidos num container de **2 vCPUs, compartilhado e ruidoso**, com Postgres e SQS
> substituídos por destinos em memória. O `fsync` desse ambiente é barato; num disco real o ganho do TCP em
> pipeline continua existindo (menos `fsync`s por ordem), mas os valores absolutos mudam. Use as **razões**
> antes/depois como referência, e rode `make bench` na sua máquina.

## Resumo

| # | Gargalo | Antes | Depois | Como |
|---|---|---|---|---|
| 1 | Anel com muitos produtores (128 goroutines) | 25 a 41 µs/ordem | **~0,2 µs/ordem** | carimbo por slot no lugar do commit encadeado |
| 2 | TCP: um `fsync` por frame, por conexão | ~13 mil ordens/s (pipeline, com `fsync`) | **~490 mil ordens/s** | confirmação em lote por conexão |
| 3 | Snapshot do livro (a cada 200 ms, sob lock) | 38 ms com 100 mil níveis | **8 µs**, independente do tamanho | só varre o topo do heap |
| 4 | Motor sob leitor de snapshot | 2,5 µs/ordem (5x mais lento) | 0,46 µs (sem leitor: 0,44) | consequência do item 3 |
| 5 | CPU ociosa | ~101% de um núcleo sem tráfego | **~11%** | espera adaptativa (gira, depois dorme 50 µs) |
| 6 | Replay do WAL no boot | 463 mil eventos/s | **1,9 milhão/s** | decodificador canônico com fallback |
| 7 | INSERT em massa do batcher | 584 µs por 1000 linhas | **225 µs** | um buffer + `strconv` |
| 8 | Candles | agregava a tabela inteira a cada 5 s | últimas 6 h + índice | (não medido, ver limites) |

## 1. Comboio de produtores no anel

**Sintoma.** `BenchmarkRing_ManyProducers`: o custo por ordem *crescia* com o número de produtores
(1 produtor: ~0,25 µs; 8: ~3 µs; 128 goroutines: ~25 a 41 µs, ou seja, ~25 mil ordens/s no total).
O profile mostrava o tempo no escalonador (`futex`, `procyield`, `schedule`), não na lógica.

**Causa.** Depois de escrever o evento, o produtor da sequência N esperava o N-1 terminar para avançar um cursor
único (CAS). Se uma goroutine era preemptida no meio, todas as seguintes ficavam girando e cedendo CPU.

**Correção.** Cada slot tem seu carimbo (`published = seq+1`). O produtor só carimba o *próprio* slot; o journaler lê
os carimbos em ordem e para no primeiro que faltar. Ninguém espera outro produtor. A ordem de sequência (e portanto a
ordem do WAL) não muda.

**Resultado** (mediana de 3 execuções): par=1 583 → 183 ns; par=8 4.847 → 149 ns; par=64 41.529 → 206 ns.
`BenchmarkRing_WaitDurableHerd` (journal instantâneo): par=64 33,4 µs → 1,1 µs.

## 2. TCP serializado por `fsync`

**Sintoma.** Um cliente em pipeline (8 conexões, 100 mil ordens) fazia ~13 mil ordens/s com `fsync` contra
~125 mil sem `fsync`: o disco era 10x mais lento mesmo com o group commit.

**Causa.** Cada conexão lia **um** frame, esperava o `fsync` dele e só então lia o próximo. O group commit só
juntava ordens de conexões *diferentes*; um cliente em pipeline nunca tinha mais de uma ordem por conexão no lote.

**Correção.** A conexão lê todos os frames que já estão no buffer, admite (valida, deduplica, publica) cada um,
espera **uma vez** o último com `WaitDurable`, e responde os ACKs em uma escrita só. Os códigos e a garantia
(ACK implica em disco) não mudam; a ordem das respostas segue a ordem dos frames.

**Resultado:** pipeline com `fsync` ~13 mil → ~490 mil ordens/s; sem `fsync` ~125 mil → ~900 mil.
No modo lock-step (uma ordem em voo por conexão) o ganho é pequeno e dentro do ruído, como esperado: não há o que agrupar.

## 3. Snapshot do livro

**Sintoma.** `GetSnapshot` ordenava **todos** os níveis de preço sob o `RLock` a cada 200 ms. Com 100 mil níveis:
38 ms parado (e o motor, que precisa do lock de escrita, esperando).

**Correção.** Os níveis ficam num heap binário; os `k` menores de um heap estão sempre nas primeiras `2^k − 1`
posições do array (profundidade ≤ k−1). Para os 10 melhores níveis basta olhar no máximo 1023 entradas por lado,
qualquer que seja o tamanho do livro. O ticker do WebSocket também pula o snapshot sem clientes e escreve nos
sockets fora do lock.

**Resultado:** 100 mil níveis 37,7 ms → 8 µs. Motor com leitor a cada 5 ms: 2.515 → 460 ns/ordem.

## 4. CPU ociosa

O consumidor do motor fazia `Gosched()` em laço, ocupando ~101% de um núcleo sem nenhuma ordem. Agora gira
256 vezes e depois dorme 50 µs (o journaler já fazia algo parecido). Medido: ~11% de um núcleo ocioso.
**Custo:** o primeiro evento depois de um período parado pode esperar até ~1 ms (pior caso medido: 1,2 ms).
Sob tráfego contínuo o caminho quente não muda. Testes: `TestIdle_*`.

## 5. Replay do WAL (boot)

`ReadAll`/`repairTail` usavam `json.Decoder` (reflection, 1 alocação por evento). Linhas no formato do próprio encoder
agora são decodificadas à mão; qualquer coisa fora do padrão cai no `json.Decoder` a partir dali, que decide o que
é íntegro. Os testes comparam o resultado com o decoder antigo em arquivos válidos, 5 mil arquivos corrompidos
aleatoriamente, casos-limite e fuzz. 463 mil → 1,9 milhão de eventos/s. Um WAL de 10 milhões de eventos
leva ~5 s para reler (antes ~22 s); o WAL continua sem truncamento nem snapshot, então esse custo ainda cresce.

## 6. Batcher

`buildInsert` usava um `fmt.Sprintf` por linha. Agora usa um buffer único e `strconv`: 584 → 225 µs por 1000 linhas.
O que sobra de alocações é o boxing dos argumentos exigido pelo `database/sql`. Testado contra a implementação antiga.

## Validação em hardware real

Medido com `make ab` (`scripts/ab-bench.sh`): Ubuntu no WSL2 sobre Windows 11, cliente e servidor na mesma máquina,
Postgres e SQS no MiniStack, `fsync` de 1,2 a 3,9 ms por escrita síncrona. Duas rodadas alternadas por versão,
100 mil ordens, todas aceitas e com ACK após o `fsync`. Na carga com janela, a latência mínima das duas versões
é um ciclo de `fsync` (~4,4 a 4,8 ms na main; 1,9 a 4,6 ms na perf, conforme o disco da rodada).

| Carga | main | perf/bottlenecks |
|---|---|---|
| TCP em pipeline, rajada (`dma-loadtest`, 16 conexões) | ~2,4 a 2,7 mil ordens/s | **~53 a 58 mil ordens/s** (~22x) |
| TCP em pipeline, janela de 32 por conexão: vazão | ~2,1 a 4,0 mil ordens/s | **~31 a 62 mil ordens/s** |
| mesma carga: latência mean / p99 | 129 a 243 ms / ~320 ms | **8 a 16 ms / 23 a 27 ms** |
| HTTP, 150 clientes, um pedido em voo por conexão | ~18 a 19 mil req/s | ~18 a 19 mil req/s (empate) |

O ganho no TCP não depende da velocidade do disco: a main ficou em ~2,5 mil/s com o disco rápido e com o lento, e a
perf em ~53 a 58 mil/s nos dois casos. No HTTP o teto é o `fsync`: com ~3,5 ms por escrita, cada ACK espera 1 a 2
ciclos (~8,5 ms) e 150 clientes dão ~17 mil/s, qualquer que seja o código. Só há ganho onde dá para agrupar mais
ordens por `fsync` (pipeline, mais conexões). Amostras pequenas (execuções de ~2 s): vale a ordem de grandeza, não o fator exato.

## Medido e deixado como está

- **Motor de matching** (`container/list` + `container/heap`): ~440 ns e 2 alocações por ordem (~2,2 M ordens/s),
  bem acima do que o `fsync` e o WAL permitem. Reescrever com slices e heap tipado ganharia algumas centenas de ns,
  com risco de regressão. Não compensa hoje. Se virar gargalo, o teste diferencial (`reference_test.go`) é a rede de segurança.
- **`WaitDurable` (rebanho de esperadores)**: depois da correção 1, ~1 µs por ordem com 128 esperadores. Não é gargalo.
- **Codificação do WAL**: ~375 ns/evento (~3,8 M eventos/s), sem alocações. Não é gargalo.
- **`Ingress.Submit`** (validação + dedupe sharded): ~330 ns em paralelo.

## Não medido / não testado

- **Candles (janela de 6 h + índice).** Exigem Postgres real; não há teste automatizado. É uma **mudança de
  comportamento**: o gráfico não mostra mais candles com mais de 6 h. A janela é a constante na consulta em `internal/api/chart.go`.
- **Publicação no SQS.** Uma mensagem por trade e descarte quando o canal enche; não foi alterada nem medida
  (não há SQS real aqui). Próximo candidato: `SendMessageBatch` (10 por chamada) e um contador de trades descartados.
- **Crescimento do conjunto de `order_id`s** (~50 MB por milhão de ordens) e **WAL sem truncamento**: dívidas de
  escala já listadas no README, não de velocidade.

## Reproduzindo

```bash
make bench                                                    # tudo
go test -run xxx -bench Ring_ManyProducers ./internal/disruptor/
go test -run xxx -bench Snapshot ./internal/matching/
go test -run xxx -bench E2E -benchtime 100000x ./internal/app/
go test -run xxx -bench Ring_ManyProducers -cpuprofile cpu.out ./internal/disruptor/ && go tool pprof cpu.out
```
