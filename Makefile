.PHONY: fmt test build server

PORT ?= 8000
USER ?= heroku-cloudwatch-drain
PASS ?= password

fmt:
	go fmt ./...

test:
	go test -race ./...

build:
	go build

server:
	go run main.go -bind=:$(PORT) -user=$(USER) -pass=$(PASS)
