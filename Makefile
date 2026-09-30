# One-terminal development loop. The two commands it runs are also documented
# in the README, which is the version to follow if you want a log per process.

.PHONY: dev backend frontend test test-race test-frontend bench

dev:
	./scripts/dev.sh

backend:
	cd backend && go run ./cmd/exchanged -addr 127.0.0.1:8080 -session-dir sessions -bots=true

frontend:
	cd frontend && npm run dev -- --host 127.0.0.1 --port 5173

test:
	cd backend && go test ./...
	cd frontend && npm test

test-race:
	cd backend && go test -race ./...

test-frontend:
	cd frontend && npm test

# Engine only. These numbers do not include the actor, the log, the network, or the UI.
bench:
	cd backend && go test ./internal/engine/ -run '^$$' -bench . -benchmem
