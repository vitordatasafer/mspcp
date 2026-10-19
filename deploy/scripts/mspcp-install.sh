#!/usr/bin/env bash
# mspcp-install.sh — instala ou atualiza o mspcp num Debian 12.
#
# Fica em /etc/scripts/mspcp-install.sh. Na primeira vez, rode a partir do
# código-fonte:
#
#   sudo bash deploy/scripts/mspcp-install.sh
#
# Depois, para atualizar (código novo na mesma pasta):
#
#   sudo /etc/scripts/mspcp-install.sh
#
# Opções:
#   -s DIR    pasta do código-fonte (padrão: a pasta de onde o script veio,
#             ou a usada na última instalação)
#   -p HOSTS  opcional: restringe os hosts do Proxmox que o portal pode
#             acessar (PROXMOX_ALLOWED_HOSTS). Os clusters são cadastrados
#             depois, pelo portal (Infraestrutura › Clusters).
#   -n NOME   nome ou IP pelo qual o portal é acessado (vai no certificado)
#   -k        não compila: usa bin/mspcp e web/dist que já estão no código
#
# O que faz: instala PostgreSQL, Go e Node (se faltarem), compila, cria o
# usuário de sistema mspcp, o banco, o certificado autoassinado, o
# /etc/mspcp/mspcp.env, os serviços systemd (mspcp-api e mspcp-worker), roda
# as migrations e, na primeira vez, cria o administrador e mostra a senha.
set -euo pipefail

GO_VERSION=1.24.7
NODE_VERSION=22.12.0
ETC=/etc/mspcp
SCRIPTS=/etc/scripts
WEB=/usr/share/mspcp/web
BIN=/usr/local/bin/mspcp

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\n\033[1;31merro:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "rode como root (sudo)."
. /etc/os-release
[ "${ID:-}" = debian ] || die "feito para Debian (encontrado: ${PRETTY_NAME:-desconhecido})."
[ "${VERSION_ID:-}" = 12 ] || echo "aviso: testado no Debian 12; este é ${PRETTY_NAME}."

SRC="" PVE_HOSTS="" PUBLIC_NAME="" SKIP_BUILD=0
while getopts "s:p:n:kh" o; do
  case "$o" in
    s) SRC=$OPTARG ;;
    p) PVE_HOSTS=$OPTARG ;;
    n) PUBLIC_NAME=$OPTARG ;;
    k) SKIP_BUILD=1 ;;
    *) sed -n '2,26p' "$0"; exit 2 ;;
  esac
done

here=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
if [ -z "$SRC" ]; then
  if [ -f "$here/../../go.mod" ]; then SRC=$(cd "$here/../.." && pwd)
  elif [ -f "$ETC/source-dir" ]; then SRC=$(cat "$ETC/source-dir")
  else die "não achei o código-fonte; use -s /caminho/do/mspcp"; fi
fi
[ -f "$SRC/go.mod" ] && [ -d "$SRC/web" ] || die "$SRC não parece o código do mspcp."

case "$(uname -m)" in
  x86_64) GOARCH=amd64 NODEARCH=x64 ;;
  aarch64) GOARCH=arm64 NODEARCH=arm64 ;;
  *) die "arquitetura $(uname -m) não suportada." ;;
esac

# ---------- pacotes ----------
log "Pacotes do sistema"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq postgresql openssl curl ca-certificates xz-utils tar >/dev/null

# ---------- ferramentas de build ----------
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

if [ "$SKIP_BUILD" -eq 0 ]; then
  export PATH=/usr/local/go/bin:/usr/local/node/bin:$PATH
  gov=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//' || true)
  if [ -z "$gov" ] || ! ver_ge "$gov" 1.24; then
    log "Instalando Go $GO_VERSION"
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  fi
  nodev=$(node --version 2>/dev/null | sed 's/^v//' || true)
  if [ -z "$nodev" ] || ! ver_ge "$nodev" 22.12; then
    log "Instalando Node.js $NODE_VERSION"
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODEARCH}.tar.xz" -o /tmp/node.txz
    rm -rf /usr/local/node && mkdir -p /usr/local/node && tar -C /usr/local/node --strip-components=1 -xJf /tmp/node.txz && rm -f /tmp/node.txz
  fi

  log "Compilando o backend (Go $(go version | awk '{print $3}'))"
  cd "$SRC"
  export GOTOOLCHAIN=local CGO_ENABLED=0
  go mod tidy
  mkdir -p bin
  go build -trimpath -ldflags="-s -w" -o bin/mspcp ./cmd/mspcp

  log "Compilando o portal (Node $(node --version))"
  cd "$SRC/web"
  npm ci --no-audit --no-fund --loglevel=error
  npm run build --silent
fi
[ -x "$SRC/bin/mspcp" ] || die "$SRC/bin/mspcp não existe (rode sem -k)."
[ -f "$SRC/web/dist/index.html" ] || die "$SRC/web/dist não existe (rode sem -k)."

# ---------- usuário, pastas e arquivos ----------
log "Usuário de sistema e pastas"
id mspcp >/dev/null 2>&1 || useradd --system --home-dir /var/lib/mspcp --create-home --shell /usr/sbin/nologin mspcp
install -d -m 0750 -o root -g mspcp "$ETC" "$ETC/tls"
install -d -m 0700 -o mspcp -g mspcp "$ETC/secrets"
install -d -m 0755 "$SCRIPTS" /usr/share/mspcp /usr/share/doc/mspcp
echo "$SRC" > "$ETC/source-dir"

install -m 0755 "$SRC/bin/mspcp" "$BIN.new" && mv -f "$BIN.new" "$BIN"
rm -rf "$WEB.new" && cp -r "$SRC/web/dist" "$WEB.new"
rm -rf "$WEB.old" && { [ -d "$WEB" ] && mv "$WEB" "$WEB.old" || true; } && mv "$WEB.new" "$WEB" && rm -rf "$WEB.old"
install -m 0644 "$SRC/README.md" /usr/share/doc/mspcp/README.md
for f in "$SRC"/deploy/scripts/*.sh; do install -m 0755 "$f" "$SCRIPTS/$(basename "$f")"; done
install -m 0644 "$SRC"/deploy/systemd/mspcp-*.service /etc/systemd/system/

# ---------- PostgreSQL ----------
log "Banco de dados"
systemctl enable --now postgresql >/dev/null
DB_PASS=""
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'mspcp'" | grep -q 1; then
  DB_PASS=$(openssl rand -hex 24)
  runuser -u postgres -- psql -q -c "CREATE ROLE mspcp LOGIN PASSWORD '$DB_PASS'"
fi
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'mspcp'" | grep -q 1; then
  runuser -u postgres -- createdb -O mspcp mspcp
fi

# ---------- configuração ----------
ENV="$ETC/mspcp.env"
if [ ! -f "$ENV" ]; then
  log "Criando $ENV"
  [ -n "$DB_PASS" ] || die "o papel mspcp já existe no PostgreSQL mas $ENV não; recrie o arquivo a partir de deploy/mspcp.env.example com a senha certa."
  sed -e "s|postgres://mspcp:SENHA@|postgres://mspcp:${DB_PASS}@|" "$SRC/deploy/mspcp.env.example" > "$ENV"
  chown root:mspcp "$ENV"; chmod 0640 "$ENV"
fi
if [ -n "$PVE_HOSTS" ]; then
  sed -i "s|^PROXMOX_ALLOWED_HOSTS=.*|PROXMOX_ALLOWED_HOSTS=${PVE_HOSTS}|" "$ENV"
fi

# ---------- chave mestra (cifra os tokens dos clusters no banco) ----------
if [ ! -f "$ETC/master.key" ]; then
  log "Chave mestra"
  ( umask 027; openssl rand -hex 32 > "$ETC/master.key" )
  chown root:mspcp "$ETC/master.key"; chmod 0640 "$ETC/master.key"
fi
grep -q '^MASTER_KEY_FILE=' "$ENV" || printf '\nMASTER_KEY_FILE=%s/master.key\n' "$ETC" >> "$ENV"

# ---------- certificado ----------
if [ ! -f "$ETC/tls/server.crt" ]; then
  log "Certificado autoassinado"
  [ -n "$PUBLIC_NAME" ] || PUBLIC_NAME=$(hostname -I | awk '{print $1}')
  san="DNS:$(hostname -f 2>/dev/null || hostname),DNS:localhost,IP:127.0.0.1"
  for ip in $(hostname -I); do san="$san,IP:$ip"; done
  if [[ "$PUBLIC_NAME" =~ ^[0-9.]+$ ]]; then san="$san,IP:$PUBLIC_NAME"; else san="$san,DNS:$PUBLIC_NAME"; fi
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
    -subj "/CN=${PUBLIC_NAME}/O=mspcp" -addext "subjectAltName=${san}" \
    -addext "extendedKeyUsage=serverAuth" \
    -keyout "$ETC/tls/server.key" -out "$ETC/tls/server.crt" 2>/dev/null
  chown root:mspcp "$ETC/tls/server.key" "$ETC/tls/server.crt"
  chmod 0640 "$ETC/tls/server.key"; chmod 0644 "$ETC/tls/server.crt"
fi

# ---------- migrations e administrador ----------
log "Migrations"
set -a; . "$ENV"; set +a
runuser -u mspcp -- "$BIN" migrate

ADMIN_FILE=/root/mspcp-admin.txt
has_provider=$(runuser -u postgres -- psql -d mspcp -tAc "SELECT count(*) FROM tenants WHERE kind = 'provider'")
if [ "$has_provider" = 0 ]; then
  log "Criando o administrador"
  out=$(runuser -u mspcp -- "$BIN" bootstrap -name "${PRODUCT_NAME:-mspclouds}" -admin admin)
  umask 077; echo "$out" > "$ADMIN_FILE"
  echo "$out"
fi

# ---------- serviços ----------
log "Serviços"
systemctl daemon-reload
systemctl enable mspcp-api mspcp-worker >/dev/null 2>&1
systemctl restart mspcp-api mspcp-worker

port=${LISTEN_ADDR##*:}
url="https://${PUBLIC_NAME:-$(hostname -I | awk '{print $1}')}"
[ "$port" = 443 ] || url="$url:$port"
for _ in $(seq 1 30); do
  if curl -fsk "https://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
if [ "${ok:-0}" != 1 ]; then
  journalctl -u mspcp-api -n 30 --no-pager || true
  die "a API não respondeu em https://127.0.0.1:${port}/healthz"
fi

cat <<EOF

  mspcp no ar: $url
  Usuário: admin $( [ -f "$ADMIN_FILE" ] && echo "(senha temporária em $ADMIN_FILE)" )

  Próximos passos, no portal (como admin):
    1. Infraestrutura › Clusters › Adicionar cluster: endereço do Proxmox e um token
       (no node: /etc/scripts/mspcp-pve-setup.sh cria o usuário, o papel e o token)
    2. No cluster: aba Storage (classes), aba Reservas de MSP
    3. Infraestrutura › Imagens: publicar templates

  Guarde uma cópia de $ETC/master.key: sem ela os tokens salvos não abrem.

  Logs:   journalctl -u mspcp-api -u mspcp-worker -f
  Config: $ENV
EOF
