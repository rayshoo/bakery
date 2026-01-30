version = $(shell git describe --tags 2>/dev/null)

ARGS = $(filter-out $@,$(MAKECMDGOALS))

imports:
	goimports -l -w $(if $(ARGS),$(ARGS),.)
.PHONY: imports

test:
	cd test && go run main.go
.PHONY: test

server:
	go run cmd/server/main.go
.PHONY: server

client:
	go run cmd/client/main.go
.PHONY: client

compose:
	go run cmd/client/main.go --config config.yaml --compose test/compose.yaml --async
.PHONY: compose

build-job:
	CGO_ENABLED=0 \
	go build -ldflags "-s -w -X main.version=$(version)" \
	-o build/job test/job/main.go
.PHONY: build-job

bake:
	docker buildx bake --allow=fs.read=.. --push --provenance false --file deploy/container/compose.yaml $(ARGS)
.PHONY: bake

agent:
	docker buildx bake --allow=fs.read=.. --push --provenance false --file deploy/container/compose.yaml dev-agent
.PHONY: agent

zrok:
	zrok share public localhost:3000
.PHONY: zrok

PASS_ARG_TARGETS := imports bake
ifneq (,$(filter $(PASS_ARG_TARGETS),$(MAKECMDGOALS)))
$(filter-out $(PASS_ARG_TARGETS),$(MAKECMDGOALS)):
	@:
endif