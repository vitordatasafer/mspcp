#!/usr/bin/env bash
# mspcp-cli.sh — roda o binário mspcp com a configuração dos serviços.
#
#   /etc/scripts/mspcp-cli.sh reset-password -user admin
#   /etc/scripts/mspcp-cli.sh cluster-add -name lab-01 -dc Lab -location "Laboratório" \
#       -url https://10.0.0.11:8006 -ca-file /etc/mspcp/lab-01-ca.pem \
#       -token-id 'mspcp@pve!portal' -secret-file -
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "rode como root (sudo)" >&2; exit 1; }
set -a
. /etc/mspcp/mspcp.env
set +a
exec /usr/local/bin/mspcp "$@"
