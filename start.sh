#!/usr/bin/env bash
# ============================================================
#  zyCtrlcenter 一键启动（Linux / macOS / Git Bash）
#  1) 构建前端面板（web/dist，缺失时；Vue3 + Vite）
#  2) 编译中控（go build -o zyctrlcenter .）
#  3) 启动中控（HTTP API :28082 / 控制通道 :27200；面板由中控直接托管）
#
#  用法：
#    ./start.sh                     构建面板（需要时）+ 编译 + 启动
#    ./start.sh --skip-web          跳过面板构建（用已有 web/dist）
#    ./start.sh --force-web         强制重新构建面板
#    ./start.sh --restart           先结束正在运行的中控再启动（pkill -x）
#    ./start.sh --auto-robot ...    其余参数原样透传给中控
#    （首次使用若提示无权限：chmod +x start.sh）
#
#  环境变量：
#    ZYROBOT_DEPLOY=<目录>    机器人部署目录；**存在才**作为 --deploy 传入，否则不传并提示
#    ZYCTRL_SKIP_WEB=1 / ZYCTRL_FORCE_WEB=1   等价于上面的命令行开关
#    GOEXE=<go 路径>          自定义 go 可执行文件
#
#  启动后打开：http://127.0.0.1:28082/
#  前端开发模式（改前端源码用）：cd web && npm install && npm run dev（http://localhost:5273）
#
#  机器人端需自备（deploy/ 不入库）：把机器人 config.py 的 ctrl_server_ip/port 指向本机 27200，
#  或用 --deploy <目录> 指定现有部署目录（--auto-robot 时可自动拉起）。
# ============================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR" || exit 1

SKIP_WEB="${ZYCTRL_SKIP_WEB:-}"
FORCE_WEB="${ZYCTRL_FORCE_WEB:-}"
RESTART=""
PASSTHRU=()

usage() {
    cat <<'EOF'
用法: ./start.sh [--skip-web] [--force-web] [--restart] [其它参数透传给中控]
  --skip-web    跳过前端构建（用已有 web/dist）
  --force-web   强制重新构建前端面板
  --restart     先结束已在运行的中控（等价 pkill -x）
  ZYROBOT_DEPLOY=<目录>  机器人部署目录，存在才作为 --deploy 传入
  启动后打开: http://127.0.0.1:28082/
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --skip-web)  SKIP_WEB=1 ;;
        --force-web) FORCE_WEB=1 ;;
        --restart)   RESTART=1 ;;
        -h|--help)   usage; exit 0 ;;
        *)           PASSTHRU+=("$1") ;;
    esac
    shift
done

# Windows（Git Bash / MSYS）下产物带 .exe 后缀
BIN="zyctrlcenter"
case "$(uname -s 2>/dev/null || echo unknown)" in
    MINGW*|MSYS*|CYGWIN*) BIN="zyctrlcenter.exe" ;;
esac

GO_BIN="${GOEXE:-go}"

echo "[INFO] zyCtrlcenter 一键启动（项目目录 $SCRIPT_DIR）"

if [ ! -f go.mod ]; then
    echo "[ERROR] 当前目录不是项目根目录（缺 go.mod）。"
    exit 1
fi

# ---- 工具检测：go ----
if ! command -v "$GO_BIN" >/dev/null 2>&1; then
    echo "[ERROR] 未找到 go，无法编译中控。"
    echo "        安装 Go（1.22+）：https://go.dev/dl/ 或设置 GOEXE=<go 路径>"
    exit 1
fi
echo "[INFO] go: $(command -v "$GO_BIN")"

if [ -z "$SKIP_WEB" ]; then
    if ! command -v npm >/dev/null 2>&1; then
        echo "[ERROR] 未找到 npm，无法构建前端面板。"
        echo "        安装 Node.js（含 npm，建议 LTS）：https://nodejs.org/"
        echo "        或跳过前端：./start.sh --skip-web"
        exit 1
    fi
    echo "[INFO] npm: $(command -v npm)"
fi

# ---- 1) 前端面板 ----
if [ -n "$SKIP_WEB" ]; then
    echo "[WEB] 已跳过前端构建（--skip-web）"
elif [ ! -f web/package.json ]; then
    echo "[ERROR] 未找到 web/package.json，无法构建面板。只想跑中控可加 --skip-web。"
    exit 1
elif [ -z "$FORCE_WEB" ] && [ -f web/dist/index.html ]; then
    echo "[WEB] web/dist 已存在，跳过构建。需要重建时加 --force-web。"
else
    (
        set -e
        cd web
        if [ ! -d node_modules ]; then
            echo "[WEB 1/2] npm install ...（首次需要下载依赖，可能较慢）"
            npm install --no-audit --no-fund
        fi
        echo "[WEB 2/2] npm run build ..."
        npm run build
    )
    if [ $? -ne 0 ] || [ ! -f web/dist/index.html ]; then
        echo "[ERROR] 前端构建失败（或未生成 web/dist/index.html）。"
        echo "        提示：刚拉取了新依赖（package.json 有变更）时，先在 web/ 下执行 npm install 再重试。"
        echo "        只想跑中控可加 --skip-web。"
        exit 1
    fi
    echo "[WEB] 面板已构建: web/dist"
fi

# ---- 2) 编译中控 ----
echo "[GO] go build -o $BIN ."
if ! "$GO_BIN" build -o "$BIN" .; then
    echo "[ERROR] go build 失败。"
    exit 1
fi
echo "[GO] 编译完成: $BIN"

# ---- 3) --deploy：只在目录确实存在时传（不写死任何绝对路径）----
DEPLOY_ARGS=()
has_deploy=""
if [ ${#PASSTHRU[@]} -gt 0 ]; then
    for a in "${PASSTHRU[@]}"; do
        case "$a" in --deploy|--deploy=*) has_deploy=1 ;; esac
    done
fi
if [ -n "$has_deploy" ]; then
    : # 用户自己传了 --deploy，原样透传
elif [ -n "${ZYROBOT_DEPLOY:-}" ] && [ -d "$ZYROBOT_DEPLOY" ]; then
    DEPLOY_ARGS=(--deploy "$ZYROBOT_DEPLOY")
    echo "[INFO] 机器人部署目录: $ZYROBOT_DEPLOY"
elif [ -n "${ZYROBOT_DEPLOY:-}" ]; then
    echo "[WARN] ZYROBOT_DEPLOY=$ZYROBOT_DEPLOY 不存在，本次不传 --deploy。"
    echo "       中控仍可启动：面板可用，机器人需自行启动并连到本机控制通道。"
else
    echo "[INFO] 未设置 ZYROBOT_DEPLOY，本次不传 --deploy（机器人进程由外部自行启动/接入）。"
fi

if [ -n "$RESTART" ]; then
    echo "[RESTART] 结束正在运行的中控 ..."
    pkill -x "$BIN" 2>/dev/null || true
    sleep 1
fi

echo
echo "[RUN] 启动中控 ...（Ctrl-C 停止）"
echo "      面板：    http://127.0.0.1:28082/    （--web-port 可改）"
echo "      控制通道：127.0.0.1:27200            （机器人连这里；--ctrl-port 可改）"
echo "      日志：    logs/ctrlcenter_YYYYMMDD.log"
echo
exec ./"$BIN" ${DEPLOY_ARGS[@]+"${DEPLOY_ARGS[@]}"} ${PASSTHRU[@]+"${PASSTHRU[@]}"}
