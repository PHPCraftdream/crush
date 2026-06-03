.PHONY: build web web-dev clean install-hooks

# Build everything: React app + Go binary with embedded assets.
build: web
	go build -o crush .

# Build only the React app into web/dist/.
web:
	cd web && npm install && npm run build

# Start React dev server (pair with: crush web --port 3030 --no-open).
web-dev:
	cd web && npm install && npm run dev

clean:
	rm -rf web/dist web/node_modules crush

# Point git at the versioned hooks in .githooks/ (run once per clone).
install-hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath=.githooks"
