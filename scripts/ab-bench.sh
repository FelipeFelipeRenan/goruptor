#!/usr/bin/env bash
# Compara versões do Goruptor (branches, tags ou hashes) sob a MESMA carga, de forma
# automática e alternada (A B A B ...), sem mexer no seu checkout:
#
#   - cada versão é compilada num git worktree temporário (você continua na sua branch);
#   - cada rodada começa com WAL novo (cada servidor roda no próprio diretório) e com a
#     tabela trade_history truncada (nada de `make nuke && make setup` entre rodadas);
#   - antes de cada rodada mede a latência de escrita síncrona do disco (dd dsync), que
#     é o que mais explica diferenças entre rodadas;
#   - no fim imprime uma tabela e grava o TSV em bench-results/.
#
# Pré-requisito: `make setup` feito UMA vez (MiniStack com SQS + Postgres no ar; o script confere as portas
# 15432 e 4566 antes de começar, PREFLIGHT=0 desliga).
#
# Uso (veja também `make ab`):
#   scripts/ab-bench.sh                              # main vs perf/bottlenecks, 2 rodadas
#   REFS="main perf/bottlenecks" ROUNDS=3 scripts/ab-bench.sh
#   LOAD=loadtest N=500000 C=16 scripts/ab-bench.sh  # TCP binário (cmd/dma-loadtest)
#   GORUPTOR_WAL_SYNC=200ms scripts/ab-bench.sh      # sem fsync por lote (isola o disco)
#
# Variáveis: REFS, ROUNDS (2), LOAD=auto|cannon|loadtest, N (100000), C (150 cannon / 16 loadtest),
#            WINDOW (loadtest: ordens em voo por conexão, 32; 0 = rajada),
#            PG_CONTAINER (ministack-rds-goruptor-db), MIN_FREE_MB (1024; espaço mínimo no container do
#            Postgres, 0 desliga), GORUPTOR_WAL_SYNC (repassada ao servidor).
set -uo pipefail

REFS=${REFS:-"main perf/bottlenecks"}
ROUNDS=${ROUNDS:-2}
LOAD=${LOAD:-auto}
N=${N:-100000}
PG_CONTAINER=${PG_CONTAINER:-ministack-rds-goruptor-db}
PG_URL="postgres://goruptor:admin123@localhost:15432/exchange?sslmode=disable"

strip_ansi() { sed -E 's/\x1b\[[0-9;]*[A-Za-z]//g'; }

# ---- leitura dos relatórios (testável: ver scripts/ab-bench.sh --selftest) ----
cannon_metric() { # <Min|Mean|p50|p99> < relatório do cannon  -> ex.: 1.48ms
  awk -F'│' -v k="$1" '$2 ~ "^ *"k {gsub(/ /,"",$3); print $3; exit}'
}
cannon_rps()  { awk '/Requests\/sec/ {print $2; exit}'; }
cannon_409()  { awk '/HTTP 409/ {print $3; f=1; exit} END {if(!f) print 0}'; }
loadtest_rate() { awk '/ordens confirmadas\/s/ {print $2; exit}'; }
loadtest_lat() { # <min|mean|p50|p95|p99|max> < saída do dma-loadtest  -> ex.: 1.23ms
  local v; v=$(sed -nE "s/.*latência.* $1=(-?[0-9.]+).*/\1/p" | head -1)
  [[ -n $v ]] && echo "${v}ms" || echo "-"
}
dd_ms_per_write() { # <writes> < saída do dd -> ms por escrita síncrona
  local w=$1 secs
  secs=$(strip_ansi | sed -nE 's/.*copied, ([0-9.,]+) s.*/\1/p' | tail -1 | tr ',' '.')
  [[ -n $secs ]] && awk -v s="$secs" -v w="$w" 'BEGIN {printf "%.2f", s/w*1000}' || echo "?"
}

selftest() {
  local r=$'│ Min          │ 1.48ms   │\n│ Mean         │ 3.35ms   │\n│ p50 (Median) │ 3.06ms   │\n│ p99          │ 7.60ms   │\n  HTTP 202                95207  ███\n  HTTP 409                 4793  █\n  Requests/sec       46494.41\n'
  [[ $(cannon_metric Min <<<"$r") == 1.48ms ]] || { echo "falhou Min"; exit 1; }
  [[ $(cannon_metric Mean <<<"$r") == 3.35ms ]] || { echo "falhou Mean"; exit 1; }
  [[ $(cannon_metric p50 <<<"$r") == 3.06ms ]] || { echo "falhou p50"; exit 1; }
  [[ $(cannon_metric p99 <<<"$r") == 7.60ms ]] || { echo "falhou p99"; exit 1; }
  [[ $(cannon_rps <<<"$r") == 46494.41 ]] || { echo "falhou rps"; exit 1; }
  [[ $(cannon_409 <<<"$r") == 4793 ]] || { echo "falhou 409"; exit 1; }
  [[ $(cannon_409 <<<"sem nada") == 0 ]] || { echo "falhou 409 vazio"; exit 1; }
  [[ $(loadtest_rate <<<"🚀 52311 ordens confirmadas/s") == 52311 ]] || { echo "falhou loadtest"; exit 1; }
  local lt='⏱️ latência (ms): min=0.50 mean=1.25 p50=1.00 p95=3.10 p99=4.20 max=9.00  (window=32 por conexão)'
  [[ $(loadtest_lat min <<<"$lt") == 0.50ms ]] || { echo "falhou lat min"; exit 1; }
  [[ $(loadtest_lat p99 <<<"$lt") == 4.20ms ]] || { echo "falhou lat p99"; exit 1; }
  [[ $(loadtest_lat mean <<<"$lt") == 1.25ms ]] || { echo "falhou lat mean"; exit 1; }
  [[ $(loadtest_lat p50 <<<"sem latência") == - ]] || { echo "falhou lat vazio"; exit 1; }
  [[ $(dd_ms_per_write 500 <<<"2048000 bytes (2.0 MB, 2.0 MiB) copied, 2.5 s, 819 kB/s") == 5.00 ]] || { echo "falhou dd"; exit 1; }
  echo "selftest ok"
}

# ---- infraestrutura ----
die() { echo "❌ $*" >&2; exit 1; }
port_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# pg_run executa SQL no Postgres: psql local se existir, senão dentro do container.
pg_run() {
  if command -v psql >/dev/null 2>&1 && psql "$PG_URL" -qc "$1" >/dev/null 2>&1; then return 0; fi
  docker exec "$PG_CONTAINER" psql -U goruptor -d exchange -qc "$1" >/dev/null 2>&1
}

truncate_pg() {
  # Dois comandos separados: CHECKPOINT não roda dentro de um bloco de transação.
  # O CHECKPOINT deixa o Postgres reciclar o WAL entre as rodadas (disco).
  pg_run "TRUNCATE trade_history" || { echo "   ⚠️  não consegui truncar trade_history (a tabela pode ainda não existir; seguindo)"; return 0; }
  pg_run "CHECKPOINT" || true
}

# Espaço livre (MB) dentro do container do Postgres; vazio se não der para medir.
pg_free_mb() {
  docker exec "$PG_CONTAINER" df -Pm /var/lib/postgresql/data 2>/dev/null | awk 'NR==2 {print $4}'
}

SERVER_PID=""
stop_server() {
  [[ -z $SERVER_PID ]] && return 0
  kill -TERM "$SERVER_PID" 2>/dev/null
  for _ in $(seq 1 100); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.2; done
  kill -0 "$SERVER_PID" 2>/dev/null && { echo "   ⚠️  servidor não parou em 20s; forçando"; kill -KILL "$SERVER_PID" 2>/dev/null; }
  wait "$SERVER_PID" 2>/dev/null
  SERVER_PID=""
}

WT_ROOT=""
cleanup() {
  stop_server
  if [[ -n $WT_ROOT && -d $WT_ROOT ]]; then
    for d in "$WT_ROOT"/*/; do git worktree remove --force "$d" >/dev/null 2>&1; done
    git worktree prune
    rm -rf "$WT_ROOT"
  fi
}

prepare_worktree() { # <ref> <dir>
  git worktree add --detach -f "$2" "$1" >/dev/null 2>&1 \
    || git worktree add --detach -f "$2" "origin/$1" >/dev/null 2>&1 \
    || die "ref '$1' não encontrada (git branch -a)"
  ( cd "$2" && go build -o goruptor-server.bin ./cmd/goruptor-server ) \
    || die "falha ao compilar '$1'"
}

run_load() { # <dir> <arquivo de saída>  -> escreve o relatório; devolve via variáveis R_*
  local dir=$1 out=$2 mode=$LOAD
  if [[ $mode == auto ]]; then command -v cannon >/dev/null 2>&1 && mode=cannon || mode=loadtest; fi
  R_MODE=$mode
  if [[ $mode == cannon ]]; then
    local c=${C:-150} side rep
    for side in BUY SELL; do
      cannon -u http://localhost:3000/api/orders -c "$N" -w "$c" -X POST \
        -H "Connection: keep-alive" -H "Content-Type: application/json" \
        --body "{\"order_id\": {{number}}, \"price\": {{number}}, \"quantity\": 1, \"side\": \"$side\"}" \
        2>&1 | strip_ansi >"$out.$side"
      rep=$(cat "$out.$side")
      printf '%s|%s|%s|%s|%s|%s\n' "$side" "$(cannon_rps <<<"$rep")" "$(cannon_metric Min <<<"$rep")" \
        "$(cannon_metric Mean <<<"$rep")" "$(cannon_metric p99 <<<"$rep")" "$(cannon_409 <<<"$rep")" >>"$out.rows"
    done
  else
    local c=${C:-16} rep
    "$LT_BIN" -n "$N" -c "$c" -window "${WINDOW:-32}" 2>&1 | strip_ansi >"$out.TCP"
    rep=$(cat "$out.TCP")
    printf 'TCP|%s|%s|%s|%s|-\n' "$(loadtest_rate <<<"$rep")" "$(loadtest_lat min <<<"$rep")" \
      "$(loadtest_lat mean <<<"$rep")" "$(loadtest_lat p99 <<<"$rep")" >>"$out.rows"
  fi
}

main() {
  [[ -d .git ]] || cd "$(git rev-parse --show-toplevel)" || die "rode dentro do repositório"
  command -v go >/dev/null || die "go não encontrado"
  for p in 3000 3001; do port_open $p && die "porta $p já está em uso (pare o servidor: Ctrl+C no make run)"; done
  if [[ ${PREFLIGHT:-1} == 1 ]]; then
    port_open 15432 || die "Postgres (porta 15432) fora do ar. Rode 'docker ps'; se o MiniStack caiu: make up (e make infra se o RDS sumiu), ou make setup."
    port_open 4566 || die "SQS/MiniStack (porta 4566) fora do ar. Rode 'make up' (ou make setup)."
  fi

  local free
  free=$(pg_free_mb)
  if [[ -n $free ]]; then
    echo "💽 espaço livre no container do Postgres: ${free} MB"
    (( free < ${MIN_FREE_MB:-1024} )) && die "pouco espaço no container do Postgres (${free} MB). No WSL/Docker o disco virtual enche: veja 'docker system df', rode 'docker system prune' ou 'make nuke && make setup'. (MIN_FREE_MB=0 ignora)"
  fi

  local stamp res_dir tsv
  stamp=$(date +%Y%m%d-%H%M%S)
  res_dir="bench-results/$stamp"; mkdir -p "$res_dir"
  tsv="$res_dir/summary.tsv"
  WT_ROOT=$(mktemp -d)
  trap cleanup EXIT INT TERM

  # O cliente de carga é o do SEU checkout, o mesmo para todas as versões: o que muda
  # entre as rodadas é só o servidor.
  LT_BIN="$WT_ROOT/dma-loadtest.bin"
  go build -o "$LT_BIN" ./cmd/dma-loadtest || die "falha ao compilar o dma-loadtest"

  echo "🔧 Compilando as versões: $REFS"
  local ref safe
  for ref in $REFS; do
    safe=${ref//\//_}
    echo "   • $ref"
    prepare_worktree "$ref" "$WT_ROOT/$safe"
  done

  printf 'rodada\tversao\tdisco_ms_por_escrita\tlado\treq_por_s\tmin\tmean\tp99\tHTTP409\tlinhas_no_WAL\n' >"$tsv"

  local round dir out disk wal
  for round in $(seq 1 "$ROUNDS"); do
    for ref in $REFS; do
      safe=${ref//\//_}; dir="$WT_ROOT/$safe"; out="$res_dir/r${round}_${safe}"
      echo "▶️  rodada $round/$ROUNDS — $ref"

      rm -f "$dir/goruptor_journal.jsonl" "$out.rows"
      truncate_pg
      free=$(pg_free_mb)
      if [[ -n $free ]] && (( free < ${MIN_FREE_MB:-1024} )); then
        echo "   ❌ só ${free} MB livres no container do Postgres; parando antes de ele cair. Veja 'docker system df' / make nuke && make setup."
        break 2
      fi

      disk=$(dd if=/dev/zero of="$dir/fs.tmp" bs=4k count=500 oflag=dsync 2>&1 | dd_ms_per_write 500)
      rm -f "$dir/fs.tmp"
      echo "   💽 disco: ${disk} ms por escrita síncrona"

      ( cd "$dir" && exec ./goruptor-server.bin ) >"$out.server.log" 2>&1 &
      SERVER_PID=$!
      for _ in $(seq 1 150); do port_open 3001 && port_open 3000 && break; kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.2; done
      if ! port_open 3001; then
        echo "   ❌ servidor não subiu; últimas linhas do log:"; tail -5 "$out.server.log"; stop_server; continue
      fi

      run_load "$dir" "$out"
      stop_server
      wal=$(wc -l <"$dir/goruptor_journal.jsonl" 2>/dev/null || echo 0)

      local side rps mn mean p99 c409
      while IFS='|' read -r side rps mn mean p99 c409; do
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$round" "$ref" "$disk" "$side" "$rps" "$mn" "$mean" "$p99" "$c409" "$wal" >>"$tsv"
        echo "   📈 $side: ${rps} req/s | min $mn | mean $mean | p99 $p99 | 409: $c409"
      done <"$out.rows"
      echo "   🗂️  ordens aceitas (linhas no WAL): $wal"
    done
  done

  echo; echo "━━━ RESUMO ($tsv) ━━━"
  column -t -s $'\t' "$tsv" 2>/dev/null || cat "$tsv"
  echo
  echo "Dica: se 'disco_ms_por_escrita' variar muito entre rodadas, a diferença é do disco, não do código."
}

case "${1:-}" in
  --selftest) selftest ;;
  *) main ;;
esac
