.PHONY: run-generators

run-generators: gen-mocks

install-tools:
	go install go.uber.org/mock/mockgen@v0.5.2

gen-mocks:
	mockgen -destination=mock.go -package stoabs -source=store.go
