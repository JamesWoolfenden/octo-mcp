BIN := octo-mcp$(shell go env GOEXE)

.PHONY: build install test vet check install-hooks clean

build: ## Build ./$(BIN)
	go build -o $(BIN) .

install: ## Install to GOPATH/bin
	go install .

test:
	go test ./...

vet:
	go vet ./...

check: vet test ## Vet, test and run every pre-commit hook
	pre-commit run --all-files

install-hooks: ## Enable the secret-scanning pre-commit hooks
	pre-commit install

clean:
	rm -f $(BIN)
