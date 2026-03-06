.PHONY: build run test fmt lint clean help ecr-login docker-push-server docker-push-worker docker-push tf-init tf-plan tf-apply tf-destroy tf-output deploy-tf-server deploy-tf-worker deploy-dev

# Build variables
BINARY_SERVER = opensandbox-server
BINARY_WORKER = opensandbox-worker
BINARY_AGENT = osb-agent
BUILD_DIR = bin
VERSION ?= $(shell cat VERSION | tr -d '[:space:]').dev
LDFLAGS_OC = -s -w -X github.com/opensandbox/opensandbox/cmd/oc/internal/commands.Version=$(VERSION)

## help: Show this help message
help:
	@grep -E '^##' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'

## build: Build server, worker, agent, and CLI binaries
build: build-server build-worker build-agent build-oc

## build-server: Build the control plane server
build-server:
	CGO_ENABLED=1 go build -o $(BUILD_DIR)/$(BINARY_SERVER) ./cmd/server

## build-worker: Build the sandbox worker
build-worker:
	CGO_ENABLED=1 go build -o $(BUILD_DIR)/$(BINARY_WORKER) ./cmd/worker

## build-worker-arm64: Cross-compile worker for Linux ARM64 (Graviton bare-metal)
build-worker-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_WORKER)-arm64 ./cmd/worker

## build-agent: Build the in-VM agent (static binary for Linux ARM64)
build-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_AGENT) ./cmd/agent

## build-agent-amd64: Build the in-VM agent (static binary for Linux x86_64)
build-agent-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_AGENT)-amd64 ./cmd/agent

## build-server-arm64: Cross-compile server for Linux ARM64
build-server-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_SERVER)-arm64 ./cmd/server

## build-oc: Build the oc CLI tool
build-oc:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS_OC)" -o $(BUILD_DIR)/oc ./cmd/oc

## install-oc: Build and install oc to $GOPATH/bin
install-oc:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS_OC)" ./cmd/oc

## --- Local Testing (3 tiers) ---

## run-dev: Tier 1 - Combined mode, no auth, no PG/NATS (simplest)
run-dev: build-server
	OPENSANDBOX_MODE=combined \
	OPENSANDBOX_API_KEY= \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data \
	$(BUILD_DIR)/$(BINARY_SERVER)

## run: Tier 1 - Combined mode with static API key
run: build-server
	OPENSANDBOX_MODE=combined \
	OPENSANDBOX_API_KEY=test-key \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data \
	$(BUILD_DIR)/$(BINARY_SERVER)

## run-pg: Tier 2 - Combined mode with PostgreSQL (requires: make infra-up)
run-pg: build-server
	OPENSANDBOX_MODE=combined \
	OPENSANDBOX_API_KEY=test-key \
	OPENSANDBOX_JWT_SECRET=dev-secret-change-me \
	OPENSANDBOX_DATABASE_URL="postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable" \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data \
	OPENSANDBOX_HTTP_ADDR=http://localhost:8080 \
	$(BUILD_DIR)/$(BINARY_SERVER)

## run-pg-workos: Tier 2 - Combined with PG + WorkOS + Vite dev (requires: make infra-up)
##   Redirect URI goes through Vite proxy so cookies are set on :3000.
##   Add http://localhost:3000/auth/callback as redirect URI in your WorkOS dashboard.
run-pg-workos: build-server
	OPENSANDBOX_MODE=combined \
	OPENSANDBOX_API_KEY=test-key \
	OPENSANDBOX_JWT_SECRET=dev-secret-change-me \
	OPENSANDBOX_DATABASE_URL="postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable" \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data \
	OPENSANDBOX_HTTP_ADDR=http://localhost:8080 \
	WORKOS_API_KEY=$${WORKOS_API_KEY} \
	WORKOS_CLIENT_ID=$${WORKOS_CLIENT_ID} \
	WORKOS_REDIRECT_URI=http://localhost:3000/auth/callback \
	WORKOS_FRONTEND_URL=$${WORKOS_FRONTEND_URL} \
	$(BUILD_DIR)/$(BINARY_SERVER)

## run-pg-workos-prod: Tier 2 - Combined with PG + WorkOS + built dashboard (requires: make infra-up web-build)
run-pg-workos-prod: build-server
	OPENSANDBOX_MODE=combined \
	OPENSANDBOX_API_KEY=test-key \
	OPENSANDBOX_JWT_SECRET=dev-secret-change-me \
	OPENSANDBOX_DATABASE_URL="postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable" \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data \
	OPENSANDBOX_HTTP_ADDR=http://localhost:8080 \
	WORKOS_API_KEY=$${WORKOS_API_KEY} \
	WORKOS_CLIENT_ID=$${WORKOS_CLIENT_ID} \
	WORKOS_REDIRECT_URI=http://localhost:8080/auth/callback \
	$(BUILD_DIR)/$(BINARY_SERVER)

## run-full-server: Tier 3 - Control plane only (requires: make infra-up)
run-full-server: build-server
	OPENSANDBOX_MODE=server \
	OPENSANDBOX_PORT=8080 \
	OPENSANDBOX_API_KEY=test-key \
	OPENSANDBOX_JWT_SECRET=dev-secret-change-me \
	OPENSANDBOX_DATABASE_URL="postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable" \
	OPENSANDBOX_NATS_URL=nats://localhost:4222 \
	OPENSANDBOX_REGION=local \
	OPENSANDBOX_WORKER_ID=cp-local-1 \
	OPENSANDBOX_HTTP_ADDR=http://localhost:8080 \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data \
	$(BUILD_DIR)/$(BINARY_SERVER)

## run-full-worker: Tier 3 - Worker only (requires: make infra-up)
run-full-worker: build-worker
	OPENSANDBOX_MODE=worker \
	OPENSANDBOX_PORT=8081 \
	OPENSANDBOX_JWT_SECRET=dev-secret-change-me \
	OPENSANDBOX_NATS_URL=nats://localhost:4222 \
	OPENSANDBOX_REGION=local \
	OPENSANDBOX_WORKER_ID=w-local-1 \
	OPENSANDBOX_HTTP_ADDR=http://localhost:8081 \
	OPENSANDBOX_DATA_DIR=/tmp/opensandbox-worker-data \
	OPENSANDBOX_MAX_CAPACITY=50 \
	$(BUILD_DIR)/$(BINARY_WORKER)

## --- Infrastructure ---

## infra-up: Start PostgreSQL + NATS via Docker Compose
infra-up:
	docker compose -f deploy/docker-compose.yml up -d
	@echo "Waiting for PostgreSQL..."
	@until docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U opensandbox 2>/dev/null; do sleep 1; done
	@echo "Infrastructure ready: PostgreSQL :5432, NATS :4222"

## infra-down: Stop infrastructure
infra-down:
	docker compose -f deploy/docker-compose.yml down

## infra-reset: Stop infrastructure and delete volumes
infra-reset:
	docker compose -f deploy/docker-compose.yml down -v

## seed: Create a test org and API key in PostgreSQL (requires: make infra-up)
seed:
	@docker compose -f deploy/docker-compose.yml exec -T postgres psql -U opensandbox -d opensandbox -c " \
		INSERT INTO orgs (name, slug) VALUES ('Test Org', 'test-org') ON CONFLICT (slug) DO NOTHING; \
		INSERT INTO api_keys (org_id, key_hash, key_prefix, name) \
		SELECT id, encode(sha256('test-key'::bytea), 'hex'), 'test-key', 'Dev Key' \
		FROM orgs WHERE slug = 'test-org' \
		ON CONFLICT (key_hash) DO NOTHING; \
	" 2>/dev/null && echo "Seeded: org=test-org, api_key=test-key" || echo "Seed failed (run migrations first by starting the server)"

## --- Web Dashboard ---

## web-dev: Start Vite dev server for dashboard (proxies to Go on :8080)
web-dev:
	cd web && npm run dev

## web-build: Build dashboard for production
web-build:
	cd web && npm install && npm run build

## build-all: Build Go binaries + web dashboard
build-all: build web-build

## --- Standard ---

## test: Run all tests
test:
	go test ./... -v -count=1

## test-unit: Run unit tests only (skip integration)
test-unit:
	go test ./... -v -count=1 -short

## test-hibernation: Run hibernation integration test (requires: server running with S3)
test-hibernation:
	@./scripts/test-hibernation.sh

## fmt: Format code
fmt:
	go fmt ./...
	goimports -w . 2>/dev/null || true

## lint: Run linter
lint:
	golangci-lint run ./... 2>/dev/null || go vet ./...

## tidy: Tidy go modules
tidy:
	go mod tidy

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -rf /tmp/opensandbox-data /tmp/opensandbox-worker-data

## proto: Regenerate protobuf/gRPC code
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/worker/worker.proto
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/agent/agent.proto

## --- Firecracker ---

## build-rootfs: Build base ext4 rootfs images for Firecracker VMs (requires root)
build-rootfs:
	sudo AGENT_BIN=$(BUILD_DIR)/$(BINARY_AGENT) IMAGES_DIR=$(BUILD_DIR)/images ./scripts/build-rootfs.sh all

## download-kernel: Download the Firecracker-compatible ARM64 kernel
download-kernel:
	./scripts/download-kernel.sh $(BUILD_DIR)/vmlinux-arm64

## deploy-worker: Deploy worker + agent to EC2 instance (set WORKER_IP=<ip>)
deploy-worker:
	./deploy/ec2/deploy-worker.sh

## --- Docker ---

## docker-server: Build server Docker image (linux/amd64 for EC2 x86_64)
docker-server:
	docker buildx build --platform linux/amd64 --load -f deploy/Dockerfile.server -t opensandbox-server .

## docker-worker: Build worker Docker image (linux/amd64 for EC2 x86_64)
docker-worker:
	docker buildx build --platform linux/amd64 --load -f deploy/Dockerfile.worker -t opensandbox-worker .

## docker: Build all Docker images
docker: docker-server docker-worker

# ECR + Push variables
AWS_REGION ?= us-east-1
AWS_ACCOUNT_ID ?= $(shell aws sts get-caller-identity --query Account --output text)
ENVIRONMENT ?= dev
ECR_SERVER_REPO = $(AWS_ACCOUNT_ID).dkr.ecr.$(AWS_REGION).amazonaws.com/opensandbox-$(ENVIRONMENT)-server
ECR_WORKER_REPO = $(AWS_ACCOUNT_ID).dkr.ecr.$(AWS_REGION).amazonaws.com/opensandbox-$(ENVIRONMENT)-worker
GIT_SHA = $(shell git rev-parse --short HEAD)
SSH_KEY_FLAG = $(if $(SSH_KEY),-i $(SSH_KEY),)

# Terraform variables
TF_DIR = deploy/terraform

## --- ECR ---

## ecr-login: Authenticate Docker with ECR
ecr-login:
	aws ecr get-login-password --region $(AWS_REGION) | docker login --username AWS --password-stdin $(AWS_ACCOUNT_ID).dkr.ecr.$(AWS_REGION).amazonaws.com

## docker-push-server: Build and push server image to ECR (git SHA + latest)
docker-push-server: docker-server ecr-login
	docker tag opensandbox-server $(ECR_SERVER_REPO):$(GIT_SHA)
	docker tag opensandbox-server $(ECR_SERVER_REPO):latest
	docker push $(ECR_SERVER_REPO):$(GIT_SHA)
	docker push $(ECR_SERVER_REPO):latest

## docker-push-worker: Build and push worker image to ECR (git SHA + latest)
docker-push-worker: docker-worker ecr-login
	docker tag opensandbox-worker $(ECR_WORKER_REPO):$(GIT_SHA)
	docker tag opensandbox-worker $(ECR_WORKER_REPO):latest
	docker push $(ECR_WORKER_REPO):$(GIT_SHA)
	docker push $(ECR_WORKER_REPO):latest

## docker-push: Build and push all images to ECR
docker-push: docker-push-server docker-push-worker

## --- Terraform ---

## tf-init: Initialize Terraform
tf-init:
	cd $(TF_DIR) && terraform init

## tf-plan: Plan Terraform changes
tf-plan:
	cd $(TF_DIR) && terraform plan

## tf-apply: Apply Terraform changes
tf-apply:
	cd $(TF_DIR) && terraform apply

## tf-destroy: Destroy Terraform resources
tf-destroy:
	cd $(TF_DIR) && terraform destroy

## tf-output: Show Terraform outputs
tf-output:
	cd $(TF_DIR) && terraform output

## --- Deploy Updates (Terraform-managed infra) ---

## deploy-tf-server: Pull latest server image and restart container on EC2
deploy-tf-server:
	@if [ -z "$(SERVER_IP)" ]; then \
		SERVER_IP=$$(cd $(TF_DIR) && terraform output -raw server_private_ip 2>/dev/null || cd $(TF_DIR) && terraform output -raw dev_host_public_ip 2>/dev/null); \
	fi; \
	echo "Deploying server to $$SERVER_IP..."; \
	ssh $(SSH_KEY_FLAG) ubuntu@$$SERVER_IP ' \
		REGION=$(AWS_REGION) && \
		aws ecr get-login-password --region $$REGION | docker login --username AWS --password-stdin $(AWS_ACCOUNT_ID).dkr.ecr.$$REGION.amazonaws.com && \
		docker pull $(ECR_SERVER_REPO):latest && \
		docker stop opensandbox-server 2>/dev/null; docker rm opensandbox-server 2>/dev/null; \
		docker run -d --name opensandbox-server --restart unless-stopped \
			-p 8080:8080 \
			--env-file /opt/opensandbox/server.env \
			--add-host host.docker.internal:host-gateway \
			$(ECR_SERVER_REPO):latest \
	'

## deploy-tf-worker: Cross-compile and SCP worker binary to EC2
deploy-tf-worker: build-worker-arm64
	@if [ -z "$(WORKER_IP)" ]; then \
		WORKER_IP=$$(cd $(TF_DIR) && terraform output -raw worker_private_ip 2>/dev/null || cd $(TF_DIR) && terraform output -raw dev_host_public_ip 2>/dev/null); \
	fi; \
	echo "Deploying worker binary to $$WORKER_IP..."; \
	scp $(SSH_KEY_FLAG) $(BUILD_DIR)/$(BINARY_WORKER)-arm64 ubuntu@$$WORKER_IP:/tmp/opensandbox-worker; \
	ssh $(SSH_KEY_FLAG) ubuntu@$$WORKER_IP 'sudo mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker && sudo chmod +x /usr/local/bin/opensandbox-worker && sudo systemctl restart opensandbox-worker'

## --- Dev Deployment (2-command workflow) ---

## deploy-dev: Full dev deployment — builds everything on-instance, no local Docker needed
##   Prerequisites: terraform apply (provisions the instance)
##   Usage: SSH_KEY=~/.ssh/opensandbox-dev.pem make deploy-dev
deploy-dev:
	@set -e; \
	DEV_IP=$$(cd $(TF_DIR) && terraform output -raw dev_host_public_ip); \
	API_KEY=$$(cd $(TF_DIR) && terraform output -raw api_key 2>/dev/null || echo "test-key"); \
	BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	SSH_CMD="ssh -o StrictHostKeyChecking=no $(SSH_KEY_FLAG) ubuntu@$$DEV_IP"; \
	echo "==> Deploying to $$DEV_IP (branch: $$BRANCH)..."; \
	\
	echo "==> Step 1: Provisioning instance (idempotent)..."; \
	$$SSH_CMD "if [ -f /usr/local/bin/firecracker ]; then echo '  Already provisioned, skipping'; else echo '  First deploy — running setup...'; fi"; \
	rsync -az --delete \
		-e "ssh -o StrictHostKeyChecking=no $(SSH_KEY_FLAG)" \
		--exclude='.git' \
		--exclude='bin/' \
		--exclude='node_modules/' \
		--exclude='web/dist/' \
		--exclude='.claude/' \
		./ ubuntu@$$DEV_IP:~/opensandbox/; \
	$$SSH_CMD "sudo bash ~/opensandbox/deploy/ec2/setup-single-host.sh"; \
	\
	echo "==> Step 2: Building server + worker + agent on instance..."; \
	$$SSH_CMD " \
		export PATH=\$$PATH:/usr/local/go/bin && \
		cd ~/opensandbox && \
		mkdir -p bin && \
		echo '  Building opensandbox-server...' && \
		CGO_ENABLED=0 go build -o bin/opensandbox-server ./cmd/server/ && \
		echo '  Building opensandbox-worker...' && \
		CGO_ENABLED=0 go build -o bin/opensandbox-worker ./cmd/worker/ && \
		echo '  Building osb-agent...' && \
		CGO_ENABLED=0 go build -o bin/osb-agent ./cmd/agent/ && \
		sudo cp bin/opensandbox-server /usr/local/bin/opensandbox-server && \
		sudo cp bin/opensandbox-worker /usr/local/bin/opensandbox-worker && \
		sudo cp bin/osb-agent /usr/local/bin/osb-agent && \
		sudo chmod +x /usr/local/bin/opensandbox-server /usr/local/bin/opensandbox-worker /usr/local/bin/osb-agent && \
		echo '  Binaries installed'"; \
	\
	echo "==> Step 3: Building rootfs (if needed)..."; \
	$$SSH_CMD " \
		if [ -f /data/firecracker/images/default.ext4 ]; then \
			echo '  Rootfs already exists, skipping (delete /data/firecracker/images/default.ext4 to rebuild)'; \
		else \
			export PATH=\$$PATH:/usr/local/go/bin && \
			cd ~/opensandbox && \
			echo '  Building rootfs with Docker (takes a few minutes)...' && \
			sudo bash ./deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default; \
		fi"; \
	\
	echo "==> Step 4: Installing env files..."; \
	$$SSH_CMD "sudo bash ~/opensandbox/deploy/ec2/setup-dev-env.sh $$API_KEY"; \
	\
	echo "==> Step 5: Starting/restarting server + worker..."; \
	$$SSH_CMD " \
		sudo systemctl restart opensandbox-server && \
		echo '  Server started' && \
		sudo systemctl restart opensandbox-worker && \
		echo '  Worker started'"; \
	\
	echo "==> Step 6: Waiting for migrations, then seeding database..."; \
	sleep 5; \
	$$SSH_CMD " \
		for i in \$$(seq 1 15); do \
			sudo docker exec postgres psql -U opensandbox -d opensandbox -q -c 'SELECT 1 FROM orgs LIMIT 0' 2>/dev/null && break; \
			echo '  Waiting for migrations...'; \
			sleep 2; \
		done && \
		sudo docker exec postgres psql -U opensandbox -d opensandbox -q -c \" \
			INSERT INTO orgs (name, slug) VALUES ('Test Org', 'test-org') ON CONFLICT (slug) DO NOTHING; \
			INSERT INTO api_keys (org_id, key_hash, key_prefix, name) \
			SELECT id, encode(sha256('$$API_KEY'::bytea), 'hex'), '$$API_KEY', 'Dev Key' \
			FROM orgs WHERE slug = 'test-org' \
			ON CONFLICT (key_hash) DO NOTHING; \
		\" && echo '  Database seeded' || echo '  WARNING: Seed failed'"; \
	\
	echo "==> Step 7: Verifying health..."; \
	sleep 3; \
	$$SSH_CMD " \
		curl -sf http://localhost:8080/health > /dev/null 2>&1 && echo '  Server health: OK' || echo '  Server health: waiting...'"; \
	\
	echo ""; \
	echo "============================================"; \
	echo " Deployment complete!"; \
	echo ""; \
	echo " Server URL: http://$$DEV_IP:8080"; \
	echo " API Key:    $$API_KEY"; \
	echo ""; \
	echo " Test:"; \
	echo "   curl -X POST http://$$DEV_IP:8080/api/sandboxes \\"; \
	echo "     -H 'Content-Type: application/json' \\"; \
	echo "     -H 'X-API-Key: $$API_KEY' \\"; \
	echo "     -d '{\"templateID\":\"default\"}'"; \
	echo ""; \
	echo " SSH:  ssh $(SSH_KEY_FLAG) ubuntu@$$DEV_IP"; \
	echo " Logs: sudo journalctl -u opensandbox-worker -f"; \
	echo "============================================"
