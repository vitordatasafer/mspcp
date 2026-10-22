# mspvdc — versão utilizável mínima

Plano de controle multi-tenant sobre Proxmox VE, no modelo MultiPortal: **admin → MSP → cliente final**, cotas sem overcommit, VDCs por localização. Esta versão já serve para uso real em laboratório:

- **Login próprio** com usuário e senha, verificação em duas etapas (TOTP), troca obrigatória da senha temporária e bloqueio por tentativas. Keycloak (OIDC) continua disponível como alternativa.
- **Criar VM a partir de imagem** (template do Proxmox com cloud-init): nome, vCPU, memória, disco, usuário, senha ou chave SSH, DHCP ou IP fixo, ligar ao criar. Tudo contra a cota do VDC.
- **Atribuir VMs existentes** a um VDC (move para o pool do Proxmox e passa a contar na cota).
- **Console no navegador** (noVNC), pelo próprio portal: o navegador nunca fala com o Proxmox.
- **Excluir VM** com confirmação pelo nome.
- **Instalação em Debian 12** com systemd e HTTPS próprio, por script.

Binário único em Go; PostgreSQL é a única dependência (a fila River roda no próprio Postgres). O portal (React) é servido pelo mesmo binário.

## Instalar no Debian 12 (laboratório)

A instalação não depende do Proxmox: os clusters são cadastrados depois, pelo portal. Numa VM Debian 12 limpa, com o código em `/root/mspvdc`:

```bash
sudo bash /root/mspvdc/deploy/scripts/mspvdc-install.sh
```

O script instala PostgreSQL, Go 1.24 e Node 22 (se faltarem), compila, cria o usuário de sistema `mspvdc`, o banco com senha aleatória, a **chave mestra** que cifra os tokens dos clusters (`/etc/mspvdc/master.key`, guarde uma cópia), um certificado autoassinado, `/etc/mspvdc/mspvdc.env` e os serviços `mspvdc-api` e `mspvdc-worker`. No fim mostra a **senha temporária do `admin`** (também em `/root/mspvdc-admin.txt`). Os scripts ficam em `/etc/scripts/`.

Acesse `https://<ip-da-vm>/` (a porta 80 redireciona), aceite o certificado e troque a senha no primeiro acesso. Para atualizar, com o código novo na mesma pasta: `sudo /etc/scripts/mspvdc-install.sh` (idempotente: mantém banco, configuração, chave, certificado e usuários).

### Cadastrar o cluster Proxmox (pelo portal)

1. **Token no Proxmox.** Copie o script para um node e rode como root; ele cria o papel `MSPVDC` (só os privilégios que o portal usa), o usuário `mspvdc@pve` (sem senha) e o token, e mostra o segredo:

   ```bash
   scp /etc/scripts/mspvdc-pve-setup.sh /etc/scripts/mspvdc-pve-template.sh root@10.0.0.11:/etc/scripts/
   ssh root@10.0.0.11 /etc/scripts/mspvdc-pve-setup.sh
   ```

   (Ou pela interface do Proxmox: usuário com o papel, token em Datacenter › Permissões › API Tokens com separação de privilégios desligada.)

2. **Infraestrutura › Clusters › Adicionar cluster.** Informe nome, datacenter, localização (o nome que MSPs e clientes veem), endereço (`https://<node>:8006`), token e segredo, e clique em **Testar conexão**. O portal mostra a versão do PVE, nodes, CPU, memória, storages, bridges, templates, os privilégios que faltarem e o certificado que vai fixar, com a impressão digital SHA-256 para conferir no node. Escolha a bridge padrão e a razão de vCPU por thread (1 = sem overcommit) e cadastre. O segredo fica cifrado no banco.

3. **Na página do cluster:**
   - **Storage:** crie as classes (SSD, HDD) e aponte cada uma para um storage do Proxmox desse cluster.
   - **Reservas de MSP:** reserve vCPU, memória e storage por classe para cada MSP. A soma das reservas não passa a capacidade do cluster (threads × razão, memória dos nodes, tamanho do storage), e uma reserva não fica abaixo do que o MSP já distribuiu em VDCs.
   - **Rede:** bridge padrão das VMs novas. **Conexão:** trocar endereço ou token (testado antes de salvar), renomear, desativar.

4. **Template:** `ssh root@10.0.0.11 /etc/scripts/mspvdc-pve-template.sh -i debian12 -s local-lvm -v 9000` (também `debian13`, `ubuntu2204`, `ubuntu2404`), depois **Infraestrutura › Imagens › Publicar**.

5. **Contas:** crie o MSP (o uso próprio da mspclouds também é um MSP, ex. "mspclouds Direto"), os limites de quantidade dele (clientes, VDCs, servidores), clientes e VDCs. Depois **Recursos › Servidores › Nova VM**, ou **Atribuir a VDC** no menu ••• de uma VM que já existe.

Diagnóstico: `journalctl -u mspvdc-api -u mspvdc-worker -f`; na página do cluster aparece o último erro de sincronização. Senha perdida: `/etc/scripts/mspvdc-cli.sh reset-password -user admin`.

### Certificado próprio

Troque `TLS_CERT_FILE` e `TLS_KEY_FILE` em `/etc/mspvdc/mspvdc.env` (o usuário `mspvdc` precisa ler os arquivos) e `systemctl restart mspvdc-api`. Atrás de um Nginx, use `LISTEN_ADDR=127.0.0.1:8080`, apague as linhas `TLS_*` e `HTTP_REDIRECT_ADDR`, e ligue `TRUST_PROXY_HEADERS=true` (o proxy precisa repassar `X-Forwarded-Proto` e aceitar WebSocket em `/v1/console/`).

## Ver funcionando sem Proxmox (demo)

```bash
make demo                               # http://localhost:8080
make demo DEMO_HOST=192.168.0.50        # https://192.168.0.50:8080 (certificado autoassinado)
```

Usa `DATABASE_URL` se definido (o `-reset` apaga o schema `public` desse banco) ou um Postgres no Docker. Sobe dois clusters PVE simulados (com nodes, storages, templates, clone, pools e um console VNC de mentira), API, worker e portal. Senha de todos: `demo12345`. Para testar o cadastro de um Proxmox de verdade na demo, rode com `PROXMOX_ALLOWED_HOSTS=<ip do node>`.

| Usuário | Papel | Vê |
| --- | --- | --- |
| vitor | super_admin | tudo, inclusive VMs sem dono, clusters e imagens |
| noc | noc_operator | todos os servidores e clusters, sem administrar |
| carla | administradora do MSP Alpha TI | Padaria Pão Quente, Lima Advocacia e o VDC de uso próprio |
| marcos | customer_admin da Padaria | só os servidores da padaria; cria VMs nos VDCs dela |
| julia | customer_admin da Clínica Sorriso | só os servidores da clínica |
| leo | viewer só no VDC Produção da Padaria | 3 servidores, sem ações |

Roteiro: como **vitor**, em **Infraestrutura › Clusters › cirion-01** veja a capacidade e as reservas de MSP (altere a da Alpha TI); no ••• de `migrar-cliente-x` use **Atribuir a VDC**. Como **marcos**, **Nova VM** no VDC Homologação, abra o **Console** quando ligar e depois exclua. Em **Minha conta**, ative o 2FA com qualquer app autenticador. Como **carla**, em Contas › Lima Advocacia, crie um usuário e entre com a senha temporária.

Sem Docker: `apt install -y postgresql && make db-local`, exporte as variáveis que ele mostra e rode `make test-e2e && make demo`.

## Regras de recursos

| Quem | Faz | Não faz |
| --- | --- | --- |
| Admin | cadastra clusters pelo portal, classes de storage, imagens e a rede de cada VDC; cria MSPs e reserva recursos para eles em cada cluster; atribui VMs existentes | — |
| MSP | cria clientes e usuários, divide a própria cota em VDCs, cria e exclui VMs | vê clusters, nodes, storages ou templates do Proxmox; muda a própria cota |
| Cliente (admin) | cria e exclui VMs nos VDCs que recebeu, liga/desliga, abre o console, gerencia os próprios usuários | cria ou aumenta VDCs; vê o MSP ou outros clientes |
| Operador | liga, desliga, reinicia, abre o console | cria ou exclui VMs |
| Leitura | vê | qualquer ação |

- **Reserva por cluster:** em cada cluster, o admin reserva para o MSP vCPU, memória e storage por classe. As localizações do MSP são os clusters onde ele tem reserva. A soma das reservas não passa a capacidade do cluster (vCPU = threads × razão definida no cluster).
- **Limites globais do MSP:** quantidade de clientes, VDCs e servidores (vazio = sem limite).
- **VDC** é uma fatia da reserva do MSP num cluster, com um resource pool no Proxmox (criado automaticamente na primeira VM).
- **Sem overcommit:** a soma dos VDCs do MSP num cluster nunca passa a reserva ali; a checagem trava a linha do MSP (há teste com pedidos em paralelo).
- **Criar VM** confere o limite de servidores do MSP, o disco livre na classe do VDC (contando criações ainda na fila) e, se for ligar ao final, vCPU e memória.
- **Ligar** é recusado se as VMs ligadas do VDC (mais os "ligar" na fila) passarem da cota.
- **Disco por classe** conta os discos criados ou atribuídos pelo portal (o storage de cada disco é traduzido para a classe).
- **Atribuir** uma VM ligada só passa se ela couber na cota do VDC de destino.
- **Nada encolhe abaixo do uso**, e VM em pool de VDC de outro cluster fica sem dono.

## Segurança

- **Senhas** com argon2id; senha temporária obrigatoriamente trocada; mínimo de 10 caracteres.
- **Sessão** em cookie `HttpOnly`, `Secure` (com HTTPS) e `SameSite=Strict`; o banco guarda só o SHA-256 do token. Expira após 8 h sem uso e 7 dias no máximo. Trocar a senha encerra as outras sessões; resetar ou remover o último acesso encerra todas.
- **CSRF:** toda requisição que altera algo exige o cabeçalho `X-MSPVDC`, que outro site não consegue enviar.
- **Tentativas:** 5 erros seguidos de um IP para um usuário bloqueiam por 1 minuto; 30 erros de um IP, por 10 minutos. Usuário inexistente gasta o mesmo tempo que senha errada.
- **2FA** TOTP (RFC 6238, 6 dígitos, janela de ±30 s), confirmado com um código antes de valer.
- **Gestão de usuários:** só reseta a senha de alguém quem pode dar todos os papéis que essa pessoa tem.
- **Senha da VM** sai do navegador e vira hash SHA-512 (`$6$`) na API; o texto puro não é gravado nem enviado ao Proxmox.
- **Console:** ticket de uso único, válido por 30 s e preso ao usuário; o WebSocket confere a origem; a API fala com o Proxmox com o token do cluster e só repassa os bytes.
- **Isolamento:** toda consulta recebe o escopo do usuário e filtra no SQL; fora do escopo é 404. Respostas para tenants não têm `vmid`, node, cluster, pool, bridge nem template.
- **Proxmox:** token cifrado no banco (AES-256-GCM, chave em `/etc/mspvdc/master.key`, fora do banco); certificado do cluster fixado no cadastro (ou CA pública validada pelo nome); cadastro só depois de testar a conexão; sem redirects nem proxy; o portal não conecta em loopback nem link-local, e `PROXMOX_ALLOWED_HOSTS` restringe ainda mais se definido.
- **Auditoria** append-only de logins, mudanças de senha e 2FA, usuários, criação, exclusão, atribuição, console e ações.
- **systemd:** serviços sem root, `ProtectSystem=strict`, só `CAP_NET_BIND_SERVICE` para as portas 80/443.

## Configuração (`/etc/mspvdc/mspvdc.env`)

| Variável | Uso | Padrão |
| --- | --- | --- |
| `DATABASE_URL` | PostgreSQL | obrigatória |
| `AUTH_MODE` | `local` (login do portal) ou `oidc` (Keycloak) | `local` |
| `PRODUCT_NAME` | nome na tela de login e no app autenticador | `mspvdc` |
| `LISTEN_ADDR` | endereço da API e do portal | `:8443` (`:443` no script) |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | HTTPS próprio; vazios = HTTP (só atrás de proxy) | vazios |
| `HTTP_REDIRECT_ADDR` | porta que só redireciona para HTTPS, ex. `:80` | vazio |
| `MASTER_KEY_FILE` | chave (64 hex) que cifra os tokens dos clusters no banco | `/etc/mspvdc/master.key` no script |
| `PROXMOX_ALLOWED_HOSTS` | opcional: restringe os hosts do Proxmox (`host[:porta]`, vírgula) | vazio (qualquer, menos loopback/link-local) |
| `SECRETS_DIR` | segredos `file:<nome>` (legado) | `/etc/mspvdc/secrets` |
| `WEB_DIR` | build do portal | vazio (só API) |
| `SYNC_INTERVAL` | intervalo do sincronizador | `20s` |
| `WORKER_CONCURRENCY` | tarefas simultâneas | `50` |
| `TRUST_PROXY_HEADERS` | `true` só atrás de proxy reverso | `false` |
| `OIDC_ISSUER`, `OIDC_AUDIENCE`, `OIDC_CLIENT_ID`, `OIDC_CA_FILE` | só com `AUTH_MODE=oidc` | — |

### Comandos

```
mspvdc migrate                         aplica migrations (o serviço já roda ao subir)
mspvdc bootstrap -name N -admin USER   cria o provedor e o 1º admin (senha temporária)
mspvdc reset-password -user USER       nova senha temporária (e desliga o 2FA)
mspvdc cluster-add -name ... -url ... -token-id ... -secret-file -   (alternativa ao portal)
mspvdc api | worker                    os serviços
```

No servidor instalado, use `/etc/scripts/mspvdc-cli.sh <comando>`: ele carrega o `mspvdc.env`.

### Privilégios do token no Proxmox

`VM.Audit VM.PowerMgmt VM.Console VM.Clone VM.Allocate VM.Config.{CDROM,CPU,Cloudinit,Disk,HWType,Memory,Network,Options} VM.GuestAgent.Audit Datastore.Audit Datastore.AllocateSpace Pool.Allocate Pool.Audit Sys.Audit SDN.Use`, em `/`. O `mspvdc-pve-setup.sh` ignora os que não existirem na versão do PVE. Pools usam a API do PVE 8.1+ (`PUT /pools` com `allow-move`), com recuo para a antiga.

### Keycloak (opcional)

`AUTH_MODE=oidc`. No realm, client público `mspvdc-portal` com Standard flow, PKCE `S256`, redirect `https://portal/callback`, web origin `https://portal` e um mapper Audience com `mspvdc-portal`. Bootstrap com `-oidc -admin <sub>`. Em "Usuários", o portal passa a vincular o `sub` do Keycloak em vez de criar usuário local.

## API v1

Login local: `POST /auth/login {username, password, totp?}` devolve o cookie de sessão (ou `{"need_totp": true}`); `POST /auth/logout`. Toda chamada que altera algo leva `X-MSPVDC: 1`. Com OIDC, `Authorization: Bearer <token>`.

| Método e rota | Quem | Faz |
| --- | --- | --- |
| `GET /v1/me` | todos | usuário, vínculos, 2FA, troca de senha pendente |
| `PUT /v1/me/password` | login local | `{current_password, new_password}` |
| `POST /v1/me/totp/setup`, `.../enable`, `.../disable` | login local | 2FA: gera segredo e URI, confirma com `{code}`, desliga com `{password}` |
| `POST /v1/tenants/{id}/users` | admins | cria usuário local com papel: `{username, name, email, role, project_id?}` → senha temporária |
| `POST /v1/users/{username}/reset-password` | admins | `{reset_totp}` → senha temporária |
| `DELETE /v1/tenants/{id}/memberships/{mid}` | admins | remove um acesso |
| `GET/POST /v1/tenants`, `GET/POST /v1/tenants/{id}/projects`, `PUT /v1/projects/{id}/quota` | — | contas e VDCs (como antes) |
| `GET /v1/tenants/{id}/capacity` | admin, o próprio MSP | reservas por localização, uso, VDCs |
| `PUT /v1/tenants/{id}/limits` | admin | `{max_customers, max_vdcs, max_vms}` |
| `POST /v1/clusters/probe` | admin | testa `{api_url, token_id, token_secret, ca_cert_pem?}` sem gravar: certificado, versão, capacidade, storages, bridges, privilégios |
| `POST /v1/clusters` | admin | cadastra (testa antes; segredo cifrado; fixa o certificado) |
| `GET/PUT /v1/clusters/{id}`, `POST /v1/clusters/{id}/sync` | equipe / admin | detalhe com capacidade, storages, bridges e reservas; editar; ler agora |
| `PUT/DELETE /v1/clusters/{id}/reservations/{msp}` | admin | reserva `{cpu_cores, memory_mb, storage[{class_id, capacity_gb}]}` |
| `PUT /v1/projects/{id}/network` | admin | `{bridge, vlan_tag}` das VMs novas do VDC |
| `POST /v1/projects/{id}/vms` | admin, MSP, cliente | cria VM: `name, image_id, cpu_cores, memory_mb, disk_gb, storage_class_id, username, password, ssh_keys, ip_mode (dhcp/static), ip_address, gateway, start` → 202 + Task |
| `DELETE /v1/resources/{id}` | admin, MSP, cliente | `{confirm_name}` → 202 + Task |
| `POST /v1/resources/{id}/assign` | admin | `{project_id}` (ou `null` para tirar do VDC) → 202 + Task |
| `POST /v1/resources/{id}/console` | operador ou acima | `{path, password}` para o noVNC (uso único, 30 s) |
| `GET /v1/console/{ticket}` | quem pediu | WebSocket do console |
| `POST /v1/resources/{id}/actions` | operador ou acima | `start, shutdown, stop, reboot` |
| `GET /v1/images?location_id=` | todos | catálogo (sem template para tenants) |
| `POST /v1/images`, `DELETE /v1/images/{id}` | admin | publica/despublica `{location_id, template_vmid, name, os_family, min_disk_gb}` |
| `GET /v1/clusters/{id}/templates` | equipe | templates vistos no cluster |
| `GET /v1/clusters`, storage classes e policies | equipe | infraestrutura |
| `GET /v1/resources`, `/v1/resources/{id}`, `/v1/projects`, `/v1/tasks`, `/v1/tasks/{id}` | todos | no escopo do usuário |

## Desenvolvimento

```bash
go mod tidy                 # uma vez (ver nota abaixo)
make test                   # unitários
make test-e2e               # + ponta a ponta com Postgres real e PVE falso
go run ./cmd/devstack -web ""   # API simulada em :8080
cd web && npm run dev            # portal com recarga em :5173 (proxy para a API e o console)
```

**Nota sobre `go.sum`:** o projeto foi montado num ambiente sem acesso a `proxy.golang.org`, então o `go.sum` não traz as linhas de `golang.org/x/*`, `gopkg.in/*` e `go.uber.org/goleak`. O script de instalação roda `go mod tidy`; em desenvolvimento, rode uma vez.

```
cmd/mspvdc/           binário: migrate | bootstrap | reset-password | cluster-add | api | worker
cmd/devstack/        demonstração: PVEs simulados + api + worker + portal
deploy/              scripts (/etc/scripts), units systemd e modelo do mspvdc.env
web/                 portal React + TypeScript (Vite), noVNC no console
internal/api/        rotas HTTP, login local, console, VMs (+ testes ponta a ponta)
internal/localauth/  argon2id, TOTP, limitador de tentativas, crypt SHA-512
internal/auth/       validação OIDC/JWKS
internal/rbac/       papéis, permissões e escopos
internal/quota/      regras de cota (VDC, MSP, ligar, criar, atribuir)
internal/store/      único lugar com SQL; queries recebem o escopo
internal/db/         pool e migrations (schema + River)
internal/proxmox/    cliente PVE: allowlist, CA por cluster, clone, pools, console
internal/inventory/  sincronizador de VMs e templates
internal/jobs/       workers River: energia, criar, excluir, atribuir (retomáveis)
internal/secrets/    referências de segredo (file:, env:)
internal/testutil/   PVE falso (com VNC), IdP falso
```

## Próximos passos

- Criar VDC já com rede própria (VNet do SDN por VDC) e reservar IPs (IPAM).
- Redimensionar VM (vCPU, memória, disco) e mais discos por VM.
- Snapshots e backup (PBS) por VDC.
- Keycloak como padrão para equipes grandes; API keys por tenant.
- Filas por DC para workers perto dos clusters; cofre Vault/OpenBao (`vault:`).
- Medição de uso para pay-as-you-go.
