.PHONY: clean security pre_commit test generate dev

GO_PACKAGES = ./...

clean:
	rm -rf ./build

security: clean
	gocritic check -enableAll ./...
	gosec ./...
	golangci-lint run ${GO_PACKAGES} --timeout=10m

pre_commit: clean
	pre-commit run --all-files
