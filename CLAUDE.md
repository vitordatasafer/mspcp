# mspcp — contexto para o Claude

Portal multi-tenant sobre Proxmox VE (modelo MultiPortal) da mspclouds.com. Dono: Vitor (infra). **Responder sempre em português.** Código, comentários, mensagens de erro e textos da interface em português do Brasil.

Leia este arquivo antes de explorar o código; abra só os arquivos que a tarefa toca.

## Regras de negócio (não mudar sem o Vitor decidir)

- Hierarquia: **provedor (admin) → MSP (partner) → cliente final (customer)**. Cliente só existe sob MSP; o uso próprio da mspclouds é um MSP ("mspclouds Direto").
- **Clusters** são cadastrados pelo portal (Infraestrutura › Clusters): testar conexão → fixar certificado → token cifrado no banco.
- **Reserva por cluster:** o admin reserva para cada MSP vCPU, memória e storage por classe num cluster (`msp_reservations`). Soma das reservas ≤ capacidade do cluster (vCPU = threads × `cpu_ratio`; memória dos nodes; tamanho do storage mapeado). Reserva nunca abaixo do que o MSP já distribuiu.
- **Limites globais do MSP:** só quantidades (clientes, VDCs, servidores) em `msp_limits`.
- **VDC** (`projects`) = fatia da reserva do MSP num cluster, com um pool no Proxmox. Soma dos VDCs do MSP no cluster ≤ reserva. **Nunca overcommit.**
- Ligar VM / criar VM ligada: recusado se passar a cota de vCPU/RAM do VDC (contando "start" e criações na fila). Disco por classe conta `resource_disks`.
- Só admin e MSP criam VDCs; cliente cria/exclui VMs nos VDCs dele; operador liga/desliga/console; viewer só vê.
- MSP e cliente **nunca** veem cluster, node, vmid, pool, bridge, storage do PVE nem template — só "localização" e nome da imagem.

## Stack e estrutura

- Go 1.24 (chi, pgx v5, River v0.28 — fila no próprio Postgres, coder/websocket, x/crypto/argon2). Binário único `cmd/mspcp`: `migrate | bootstrap | reset-password | cluster-add | api | worker`.
- `cmd/devstack`: demo com 2 PVEs simulados (`internal/testutil/fakepve.go`, com VNC falso), usuários prontos (senha `demo12345`).
- Portal: React 19 + Vite 8 + TS + TanStack Query, em `web/src` (pages/, components/, `api.ts` tipos e `api()`, `session.tsx` permissões de UI). Visual no padrão ZSvirt (topbar #0043B1, trilho de ícones, subnav, rodapé de tarefas; tokens em `styles.css`). noVNC 1.4 (CommonJS; ver interop em `pages/Console.tsx`).

| Pasta | O quê |
| --- | --- |
| `internal/api` | rotas (`server.go`), handlers por área (`clusters.go`, `vms.go`, `vdc.go`, `localauth.go`, `console.go`), testes e2e |
| `internal/store` | **único lugar com SQL**; toda query de usuário recebe `Scope` e filtra no SQL |
| `internal/quota` | todas as regras de cota (CheckVDC, SetReservation, CheckCreate, CheckAssign, CheckPowerOn) |
| `internal/rbac` | papéis → permissões → escopo |
| `internal/proxmox` | cliente PVE (allowlist/SSRF, certificado fixado, clone, pools, console, `Read` do inventário, `ProbeTLS`) |
| `internal/jobs` | workers River: `PowerWorker`, `VMWorker` (create/delete/assign retomáveis via `params` da task) |
| `internal/inventory` | sincronizador (VMs, templates, nodes, storages, bridges) |
| `internal/localauth` | argon2id, TOTP, limitador, crypt SHA-512 da senha cloud-init |
| `internal/secrets` | `Box` AES-GCM (chave `MASTER_KEY_FILE`), refs `file:`/`env:` legadas |
| `internal/db/migrations` | SQL numerado (`000N_nome.sql`), aplicado em ordem; nunca editar uma já publicada |
| `deploy/` | `scripts/` (vão para `/etc/scripts/` no servidor), units systemd, `mspcp.env.example` |

## Convenções

- Scripts, units e arquivos de apoio nos servidores ficam em **`/etc/scripts/`** (padrão do Vitor). Config em `/etc/mspcp/`.
- Fora do escopo do usuário → **404** (não revelar que existe); sem permissão dentro do escopo → 403; regra de cota → **422** com mensagem para o usuário (`quota.Error`).
- Tudo que altera estado: transação + `audit_log` (append-only). Task + job River na mesma transação.
- Login local: cookie `mspcp_session` HttpOnly/SameSite=Strict; requisição que altera estado exige cabeçalho `X-MSPCP`.
- Mudança de schema = nova migration + ajuste em `store` + teste.
- Textos curtos e diretos na interface; sem jargão interno para MSP/cliente.

## Build e testes

```bash
go mod tidy                      # primeira vez (go.sum incompleto de propósito)
make test                        # unitários
TEST_DATABASE_URL=postgres://... make test-e2e   # + e2e (apaga o schema public do banco!)
make demo                        # devstack em http://localhost:8080
cd web && npm run build          # portal (tsc + vite)
```

Testes principais: `internal/api/e2e_test.go` (isolamento e cotas, OIDC) e `internal/api/usable_test.go` (login/2FA, clusters/reservas, criar/excluir/atribuir VM, console). Ao mudar regra de cota ou API, atualize-os.

## Como trabalhar aqui (economia de tokens)

- Uma sessão por funcionalidade; leia só os arquivos envolvidos.
- Verificação padrão: `go vet`, testes Go e `tsc`. **Teste de navegador/screenshots só quando o Vitor pedir** ou a tela for nova.
- Mudança de regra de negócio: proponha em poucas linhas e espere o OK antes de codar.
- Entrega por commit/PR com resumo curto; sem zip.

## Estado atual e próximos passos

Pronto: login local + 2FA, usuários, clusters pelo portal, reservas por cluster, VDCs, imagens, criar/excluir/atribuir VM, console noVNC, instalação Debian 12 (`deploy/scripts/mspcp-install.sh`). Validado só contra PVE simulado — primeiro teste real será no cluster de laboratório.

Candidatos: redimensionar VM e discos extras; rede por VDC (VNet SDN/IPAM); snapshots e backup (PBS); Keycloak; API keys por tenant; medição para pay-as-you-go.
