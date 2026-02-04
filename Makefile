# 版本信息
VERSION ?= $(shell git describe --tags --always)
LDFLAGS := -s -w -X 'gost-panel/internal/config.Version=$(VERSION)'

.PHONY: all build build-web build-server clean dev help release linux linux-pack

# 默认目标
all: build

# 帮助信息
help:
	@echo "Available commands:"
	@echo "  make build          - Build both web and server"
	@echo "  make build-web      - Build web frontend only"
	@echo "  make build-server   - Build server backend only"
	@echo "  make linux          - Build Linux versions (amd64 + arm64)"
	@echo "  make linux-pack     - Build and package for deployment"
	@echo "  make dev            - Run in development mode"
	@echo "  make run            - Build web and run server"
	@echo "  make clean          - Clean build artifacts"
	@echo "  make release        - Build multi-platform release"
	@echo ""
	@echo "💡 Tip for Windows users: Run these commands in Git Bash or WSL for compatibility."

# 完整构建
build: build-web build-server
	@echo "Build complete! Binary: gost-panel"

# 构建前端
build-web:
	@echo "Building web..."
	cd web && npm install && npm run build
	@echo "Web build complete"
	
# 构建后端（包含嵌入的前端）
build-server:
	@echo "Building server..."
	go build -ldflags="$(LDFLAGS)" -o gost-panel cmd/server/main.go
	@echo "Server build complete"

# 运行（构建前端并启动后端）
run: build-web
	@echo "Starting server..."
	go run cmd/server/main.go

# 构建多平台发布版本
release: build-web
	@echo "Building release binaries..."
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o gost-panel-linux-amd64 cmd/server/main.go
	GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o gost-panel-linux-arm64 cmd/server/main.go
	GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o gost-panel-darwin-amd64 cmd/server/main.go
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o gost-panel-darwin-arm64 cmd/server/main.go
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o gost-panel-windows-amd64.exe cmd/server/main.go
	@echo "Release build complete"

# 编译 Linux 版本（用于生产部署）
linux: build-web
	@echo "Building for Linux amd64..."
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o gost-panel-linux-amd64 cmd/server/main.go
	@echo "Building for Linux arm64..."
	GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o gost-panel-linux-arm64 cmd/server/main.go
	@echo "Linux builds complete!"
	@echo "Files: gost-panel-linux-amd64, gost-panel-linux-arm64"

# 编译并打包 Linux 版本（带压缩）
linux-pack: linux
	@echo "Packaging Linux builds..."
	@mkdir -p dist
	tar -czf dist/gost-panel-linux-amd64.tar.gz gost-panel-linux-amd64 README.md LICENSE
	tar -czf dist/gost-panel-linux-arm64.tar.gz gost-panel-linux-arm64 README.md LICENSE
	@echo "Packages created in dist/"
	@ls -lh dist/*.tar.gz

# 清理构建产物
clean:
	@echo "Cleaning artifacts..."
	rm -f gost-panel
	rm -f gost-panel.exe
	rm -f gost-panel-linux-*
	rm -f gost-panel-darwin-*
	rm -f gost-panel-windows-*
	rm -f main
	rm -f main.exe
	rm -rf internal/router/dist
	rm -rf web/dist
	rm -rf dist
