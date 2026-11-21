#!/bin/sh
# ciorch 编排器安装脚本（REDESIGN §3.14）——幂等；**不含任何凭据**：
# 凭据文件由维护者手动放置，本脚本只检查存在与权限（0600），缺了报错退出。
# 前置：Ubuntu 24.04；本脚本以 root 运行。
set -eu

CiorchBin=/usr/local/bin/ciorch
EtcDir=/etc/ciorch
StateDir=/var/lib/ciorch
UnitFile=/etc/systemd/system/ciorch.service

fail() { echo "install.sh: $*" >&2; exit 1; }

# 1) 系统用户（无登录 shell）
if ! id ciorch >/dev/null 2>&1; then
    useradd --system --home-dir "$StateDir" --shell /usr/sbin/nologin ciorch
fi

# 2) 目录与权限
install -d -m 0750 -o ciorch -g ciorch "$EtcDir"
install -d -m 0700 -o ciorch -g ciorch "$StateDir"

# 3) 二进制：从 CI 产物或本仓构建产物安装（调用方放到位后运行本脚本）
[ -x "$CiorchBin" ] || fail "缺少 $CiorchBin（先放置 ciorch 二进制）"

# 4) 配置
[ -f "$EtcDir/ciorch.json" ] || fail "缺少 $EtcDir/ciorch.json（维护者手动放置）"

# 5) 凭据存在与权限检查（0600；缺了报错——不放示例文件，防占位符上线）
for f in app.pem webhook.secret status.token; do
    p="$EtcDir/$f"
    if [ -f "$p" ]; then
        perm=$(stat -c '%a' "$p")
        [ "$perm" = "600" ] || fail "$p 权限是 $perm，须 0600（chmod 600）"
        chown ciorch:ciorch "$p"
    else
        case "$f" in
        app.pem) fail "缺少 $p（GitHub App 私钥）" ;;
        webhook.secret) echo "提示：无 $p —— webhook 将不启用（阶段 4 首次部署可接受）" ;;
        status.token) echo "提示：无 $p —— /status 将拒绝所有请求（可接受）" ;;
        esac
    fi
done

# 6) systemd unit
install -m 0644 "$(dirname "$0")/ciorch.service" "$UnitFile"
systemctl daemon-reload
systemctl enable ciorch.service

echo "安装完成。启动： systemctl start ciorch"
echo "ufw 入站（如未配）： ufw allow 22,80,443/tcp && ufw enable（见 deploy/README.md）"
