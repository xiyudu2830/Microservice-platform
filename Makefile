.PHONY: test proto demo-up demo demo-down

test:
	go test -race ./...
	go vet ./...

proto:
	protoc --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. api/governance/v1/governance.proto

demo-up:
	bash scripts/init-demo.sh
	docker compose build broker
	docker compose up -d --no-build --wait --wait-timeout 120

demo:
	bash scripts/demo.sh

demo-down:
	docker compose down
