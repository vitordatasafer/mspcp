#!/usr/bin/env bash
# mspcp-pve-template.sh — cria um template cloud-init num node Proxmox,
# pronto para ser publicado como imagem no portal.
#
#   /etc/scripts/mspcp-pve-template.sh -i debian12 -s local-lvm [-v 9000] [-b vmbr0]
#   /etc/scripts/mspcp-pve-template.sh -i ubuntu2404 -s ceph-ssd -v 9001
#
# Baixa a imagem oficial de nuvem, cria a VM com disco importado, drive
# cloud-init e VGA padrão (o console do portal usa VNC) e converte em
# template. Precisa de acesso à internet no node.
set -euo pipefail

IMG="" STORAGE="" VMID="" BRIDGE=vmbr0
while getopts "i:s:v:b:h" o; do
  case "$o" in
    i) IMG=$OPTARG ;;
    s) STORAGE=$OPTARG ;;
    v) VMID=$OPTARG ;;
    b) BRIDGE=$OPTARG ;;
    *) sed -n '2,11p' "$0"; exit 2 ;;
  esac
done
[ -n "$IMG" ] && [ -n "$STORAGE" ] || { sed -n '2,11p' "$0"; exit 2; }
[ "$(id -u)" -eq 0 ] || { echo "rode como root" >&2; exit 1; }
command -v qm >/dev/null || { echo "qm não encontrado: rode num node do Proxmox VE" >&2; exit 1; }

ARCH=amd64
case "$IMG" in
  debian12)   URL="https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-${ARCH}.qcow2"; NAME=tpl-debian-12 ;;
  debian13)   URL="https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-${ARCH}.qcow2"; NAME=tpl-debian-13 ;;
  ubuntu2204) URL="https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-${ARCH}.img"; NAME=tpl-ubuntu-2204 ;;
  ubuntu2404) URL="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-${ARCH}.img"; NAME=tpl-ubuntu-2404 ;;
  *) echo "imagem desconhecida: $IMG (use debian12, debian13, ubuntu2204 ou ubuntu2404)" >&2; exit 2 ;;
esac
[ -n "$VMID" ] || VMID=$(pvesh get /cluster/nextid)
if qm status "$VMID" >/dev/null 2>&1; then echo "o VMID $VMID já existe" >&2; exit 1; fi

FILE=/var/tmp/${NAME}.img
echo "Baixando $URL"
curl -fL --retry 3 -o "$FILE" "$URL"

echo "Criando a VM $VMID ($NAME) em $STORAGE"
qm create "$VMID" --name "$NAME" --ostype l26 --cores 1 --memory 1024 \
  --net0 "virtio,bridge=${BRIDGE}" --scsihw virtio-scsi-single --agent enabled=1 \
  --cpu x86-64-v2-AES --vga std
qm set "$VMID" --scsi0 "${STORAGE}:0,import-from=${FILE},discard=on,ssd=1"
qm set "$VMID" --ide2 "${STORAGE}:cloudinit" --boot order=scsi0
qm set "$VMID" --ciuser debian --ipconfig0 ip=dhcp
qm template "$VMID"
rm -f "$FILE"

echo
echo "Template $NAME ($VMID) pronto. Em até um minuto ele aparece no portal em"
echo "Infraestrutura > Imagens; clique em Publicar para liberar aos clientes."
