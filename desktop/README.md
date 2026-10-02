# Atom2Api Desktop（Tauri 桌面版）

用 Tauri 2 把 Atom2Api 包成 Windows 桌面应用：启动后自动以 sidecar 方式拉起内置的 `atom2api` 服务进程，等服务端口就绪后打开原生窗口加载管理控制台（同 `http://127.0.0.1:8080`）。关闭窗口退出应用时会一并结束服务进程。

## 行为说明

- 服务进程以桌面程序同级目录为工作目录，`config.json` 与 `data/` 都生成在该目录旁边，随装随用。
- 若 8080 端口已有 Atom2Api 实例在跑，桌面版不会重复启动服务，窗口直接显示已有实例的控制台。
- 安装包（NSIS）默认按当前用户安装（`%LOCALAPPDATA%\Programs`），无需管理员权限。

## 构建要求

- Node.js 20+、Rust stable（`x86_64-pc-windows-msvc`）、MSVC Build Tools、WebView2 运行时（Win10/11 一般自带）。

## 构建步骤

```bash
# 1. 先构建 Go 服务并放入 sidecar 目录
cd ..
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=<版本>" -o desktop/src-tauri/binaries/atom2api-x86_64-pc-windows-msvc.exe .

# 2. 构建桌面应用（产物在 desktop/src-tauri/target/release/bundle/nsis/）
cd desktop
npm install
npx tauri build
```

产物：

- `Atom2Api_<版本>_x64-setup.exe`：NSIS 安装包（含服务 sidecar）。
- `target/release/Atom2Api.exe`：裸壳程序，需与 `binaries/` 下的服务 exe 同目录分发。

## 源码结构

- `src-tauri/src/main.rs`：sidecar 启动、端口轮询、窗口创建。
- `src-tauri/tauri.conf.json`：打包配置（`externalBin` 指向 Go 服务二进制）。
- `ui/index.html`：占位前端资源（实际界面来自运行中的服务）。
