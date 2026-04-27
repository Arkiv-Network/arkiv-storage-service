BIN     := arkiv-storaged
CMD     := ./cmd/arkiv-storaged
OUT     := ./bin/$(BIN)

.PHONY: build install test lint clean

build:
	go build -o $(OUT) $(CMD)

install:
	go install $(CMD)

test: build
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf ./bin
