#!/usr/bin/env bash
# mspvdc-pve-setup.sh — prepara um cluster Proxmox VE para o mspvdc.
#
# Rode como root em UM node do cluster (a configuração de usuários do PVE é
# do cluster todo). Guarde em /etc/scripts/ no node.
#
#   /etc/scripts/mspvdc-pve-setup.sh [-u mspvdc@pve] [-t portal]
#
# Cria:
#   - o papel MSPVDC, só com os privilégios que o portal usa;
#   - o usuário mspvdc@pve (sem senha: só entra por token);
#   - a permissão do papel em / para o usuário;
#   - o token de API (privsep desligado: herda o papel do usuário).
# E mostra o que informar no portal (Infraestrutura › Clusters).
set -euo pipefail

USER_ID=mspvdc@pve
TOKEN=portal
NAME=$(hostname -s)
while getopts "u:t:n:h" o; do
  case "$o" in
    u) USER_ID=$OPTARG ;;
    t) TOKEN=$OPTARG ;;
    n) NAME=$OPTARG ;;
    *) sed -n '2,17p' "$0"; exit 2 ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "rode como root" >&2; exit 1; }
command -v pveum >/dev/null || { echo "pveum não encontrado: rode num node do Proxmox VE" >&2; exit 1; }

# Privilégios que o portal usa. Os que não existirem nesta versão do PVE
# (os nomes mudam entre versões) são ignorados.
WANT="VM.Audit VM.PowerMgmt VM.Console VM.Clone VM.Allocate VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit
VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit
Datastore.Audit Datastore.AllocateSpace Pool.Allocate Pool.Audit Sys.Audit SDN.Use"

ALL=$(pveum role list --output-format json | python3 -c '
import json, sys
privs = set()
for r in json.load(sys.stdin):
    p = r.get("privs") or ""
    privs.update(p if isinstance(p, list) else p.replace(" ", ",").split(","))
print(" ".join(sorted(x for x in privs if x)))')
PRIVS=""
for p in $WANT; do
  if [[ " $ALL " == *" $p "* ]]; then PRIVS="$PRIVS,$p"; else echo "  (privilégio $p não existe nesta versão, ignorado)"; fi
done
PRIVS=${PRIVS#,}

if pveum role list --output-format json | python3 -c 'import json,sys; sys.exit(0 if any(r["roleid"]=="MSPVDC" for r in json.load(sys.stdin)) else 1)'; then
  pveum role modify MSPVDC --privs "$PRIVS"
  echo "Papel MSPVDC atualizado."
else
  pveum role add MSPVDC --privs "$PRIVS"
  echo "Papel MSPVDC criado."
fi

if pveum user list --output-format json | python3 -c "import json,sys; sys.exit(0 if any(u['userid']=='$USER_ID' for u in json.load(sys.stdin)) else 1)"; then
  echo "Usuário $USER_ID já existe."
else
  pveum user add "$USER_ID" --comment "Portal mspvdc (só token, sem senha)"
  echo "Usuário $USER_ID criado."
fi
pveum acl modify / --users "$USER_ID" --roles MSPVDC
echo "Permissão MSPVDC em / para $USER_ID."

if pveum user token list "$USER_ID" --output-format json | python3 -c "import json,sys; sys.exit(0 if any(t['tokenid']=='$TOKEN' for t in json.load(sys.stdin)) else 1)"; then
  echo
  echo "O token $USER_ID!$TOKEN já existe e o segredo só aparece na criação."
  echo "Para gerar outro: pveum user token remove $USER_ID $TOKEN  e rode este script de novo."
  exit 0
fi
SECRET=$(pveum user token add "$USER_ID" "$TOKEN" --privsep 0 --comment "mspvdc" --output-format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["value"])')

IP=$(hostname -I | awk '{print $1}')
echo
echo "================================================================"
echo " Token criado: $USER_ID!$TOKEN"
echo " Segredo (aparece só agora): $SECRET"
echo "================================================================"
echo
echo "Storages (para mapear as classes SSD/HDD no portal):"
pvesm status 2>/dev/null | awk 'NR==1 || /active/ {print "  " $0}'
echo
echo "Bridges deste node:"
ip -br link show type bridge 2>/dev/null | awk '{print "  " $1}'
echo
echo "No portal: Infraestrutura › Clusters › Adicionar cluster"
echo "  Endereço: https://${IP}:8006"
echo "  Token:    ${USER_ID}!${TOKEN}"
echo "  Segredo:  o mostrado acima"
echo
echo "Para conferir o certificado que o portal vai fixar:"
echo "  openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pve-ssl.pem"
