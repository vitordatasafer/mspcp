.PHONY: build web test test-e2e lint run-api run-worker demo db-local install

# Bancos usados pelos testes e pela demonstração. Sem Docker, aponte para um
# PostgreSQL local (veja `make db-local`) e exporte as variáveis:
#   export TEST_DATABASE_URL=postgres://mspcp:mspcp@127.0.0.1:5432/mspcp_test?sslmode=disable
#   export DATABASE_URL=postgres://mspcp:mspcp@127.0.0.1:5432/mspcp_demo?sslmode=disable
DOCKER_TEST_DB = postgres://mspcp:mspcp@127.0.0.1:55432/mspcp_test?sslmode=disable
DOCKER_DEMO_DB = postgres://mspcp:mspcp@127.0.0.1:55433/mspcp_demo?sslmode=disable
HAS_DOCKER := $(shell command -v docker 2>/dev/null)

define NO_DB
Nem TEST_DATABASE_URL/DATABASE_URL definido nem Docker instalado.
Instale o PostgreSQL local e rode `make db-local` (instruções no README).
endef
export NO_DB

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/mspcp ./cmd/mspcp

test:
	go test -race ./...

# Testes unitários + ponta a ponta. Usa TEST_DATABASE_URL se existir
# (o schema public desse banco é apagado); senão sobe um Postgres no Docker.
test-e2e:
ifneq ($(TEST_DATABASE_URL),)
	go test -race -count=1 ./...
else ifneq ($(HAS_DOCKER),)
	docker run -d --rm --name mspcp-testdb -p 55432:5432 \
		-e POSTGRES_USER=mspcp -e POSTGRES_PASSWORD=mspcp -e POSTGRES_DB=mspcp_test postgres:16
	until docker exec mspcp-testdb pg_isready -U mspcp >/dev/null 2>&1; do sleep 1; done
	TEST_DATABASE_URL="$(DOCKER_TEST_DB)" go test -race -count=1 ./... ; status=$$?; \
		docker stop mspcp-testdb >/dev/null; exit $$status
else
	@echo "$$NO_DB"; exit 1
endif

# Cria usuário e bancos num PostgreSQL local (sem Docker). Precisa de sudo
# para o usuário postgres; rode uma vez.
db-local:
	sudo -u postgres psql -v ON_ERROR_STOP=1 -c "DO \$$\$$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mspcp') THEN CREATE ROLE mspcp LOGIN PASSWORD 'mspcp'; END IF; END\$$\$$;"
	for db in mspcp mspcp_test mspcp_demo; do \
		sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname = '$$db'" | grep -q 1 || \
		sudo -u postgres createdb -O mspcp $$db; \
	done
	@echo
	@echo "Pronto. Exporte:"
	@echo "  export TEST_DATABASE_URL=postgres://mspcp:mspcp@127.0.0.1:5432/mspcp_test?sslmode=disable"
	@echo "  export DATABASE_URL=postgres://mspcp:mspcp@127.0.0.1:5432/mspcp_demo?sslmode=disable"

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

run-api:
	go run ./cmd/mspcp api

run-worker:
	go run ./cmd/mspcp worker

web:
	cd web && npm ci && npm run build

# Tudo local, sem Proxmox nem Keycloak: clusters e login simulados.
# Portal em http://localhost:8080, ou em http://$(DEMO_HOST):8080 com
# DEMO_HOST=<ip da VM>. Usa DATABASE_URL se existir (o schema public desse
# banco é apagado); senão sobe um Postgres no Docker.
DEMO_FLAGS = $(if $(DEMO_HOST),-public $(DEMO_HOST),)
demo: web
ifneq ($(DATABASE_URL),)
	go run ./cmd/devstack -reset $(DEMO_FLAGS) -db "$(DATABASE_URL)"
else ifneq ($(HAS_DOCKER),)
	docker run -d --rm --name mspcp-demodb -p 55433:5432 \
		-e POSTGRES_USER=mspcp -e POSTGRES_PASSWORD=mspcp -e POSTGRES_DB=mspcp_demo postgres:16 >/dev/null || true
	until docker exec mspcp-demodb pg_isready -U mspcp >/dev/null 2>&1; do sleep 1; done
	go run ./cmd/devstack -reset $(DEMO_FLAGS) -db "$(DOCKER_DEMO_DB)"
else
	@echo "$$NO_DB"; exit 1
endif

# Instala ou atualiza neste servidor (Debian 12): systemd, HTTPS, banco.
install:
	sudo bash deploy/scripts/mspcp-install.sh $(if $(PVE_HOSTS),-p $(PVE_HOSTS),)
