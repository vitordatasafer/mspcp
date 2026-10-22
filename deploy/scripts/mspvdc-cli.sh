#!/usr/bin/env bash
# mspvdc-cli.sh — roda o binário mspvdc com a configuração dos serviços.
#
#   /etc/scripts/mspvdc-cli.sh reset-password -user admin
#   /etc/scripts/mspvdc-cli.sh cluster-add -name lab-01 -dc Lab -location "Laboratório" \
#       -url https://10.0.0.11:8006 -ca-file /etc/mspvdc/lab-01-ca.pem \
#       -token-id 'mspvdc@pve!portal' -secret-file -
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "rode como root (sudo)" >&2; exit 1; }
set -a
. /etc/mspvdc/mspvdc.env
set +a
exec /usr/local/bin/mspvdc "$@"
