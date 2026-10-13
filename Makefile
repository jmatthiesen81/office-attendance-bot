# Go is not required on the host: test and lint run inside the official image.
GO_IMAGE ?= golang:1.27
DOCKER_GO = docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache \
	-v "$(CURDIR)":/src -w /src $(GO_IMAGE)

.PHONY: test vet fmt build up pull down logs status backup

test:
	$(DOCKER_GO) go test -race -count=1 ./...

vet:
	$(DOCKER_GO) sh -c 'test -z "$$(gofmt -l .)" && go vet ./...'

fmt:
	$(DOCKER_GO) gofmt -w .

build:
	docker compose build

up:
	docker compose up -d --build

# Run the published image from ghcr.io instead of building from source.
pull:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f officebot

status:
	docker compose exec officebot /officebot status

# Consistent online backup via SQLite's VACUUM INTO (safe while the bot runs).
# The file appears in ./volumes/officebot and is then moved to ./backup.
backup:
	mkdir -p backup
	name=officebot-$$(date +%Y%m%d-%H%M%S).db && \
	docker compose exec officebot /officebot backup /data/$$name && \
	mv volumes/officebot/$$name backup/$$name && echo "backup/$$name"
