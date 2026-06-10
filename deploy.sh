#!/usr/bin/env bash
set -Eeuo pipefail

SELF_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

APP_NAME="${APP_NAME:-video-site-91}"
BACKEND_SERVICE="${BACKEND_SERVICE:-video-site-backend}"
FRONTEND_SERVICE="${FRONTEND_SERVICE:-video-site-frontend}"
FRONTEND_HOST="${FRONTEND_HOST:-0.0.0.0}"
FRONTEND_PORT="${FRONTEND_PORT:-9191}"
BACKEND_LISTEN="${BACKEND_LISTEN:-127.0.0.1:9192}"
GO_VERSION="${GO_VERSION:-1.23.12}"
INSTALL_DEPS="${INSTALL_DEPS:-1}"
CONFIGURE_UFW="${CONFIGURE_UFW:-1}"

export PATH="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

log() {
  printf '\033[1;34m[deploy]\033[0m %s\n' "$*"
}

warn() {
  printf '\033[1;33m[deploy]\033[0m %s\n' "$*" >&2
}

die() {
  printf '\033[1;31m[deploy]\033[0m %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage: sudo bash deploy.sh [install|update|restart|stop|status|logs|uninstall]

Default action:
  install    Install dependencies, build, create systemd services, and start.

Actions:
  install    First deployment or full repair
  update     Rebuild current code and restart services
  restart    Restart systemd services
  stop       Stop systemd services
  status     Show service status
  logs       Follow backend and frontend logs
  uninstall  Remove systemd services only; keep repo, config, and data

Common overrides:
  FRONTEND_PORT=9191      Public web port
  FRONTEND_HOST=0.0.0.0   Public web bind address
  GO_VERSION=1.23.12
  INSTALL_DEPS=0          Do not install missing Node/Go/ffmpeg/Python runtime deps
  CONFIGURE_UFW=0         Do not open UFW port automatically
  DEPLOY_USER=<user>      Service user; defaults to sudo user or root

Examples:
  sudo bash deploy.sh
  FRONTEND_PORT=8080 sudo -E bash deploy.sh
  sudo bash deploy.sh update
  sudo bash deploy.sh logs
EOF
}

need_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    return
  fi
  if ! command -v sudo >/dev/null 2>&1; then
    die "this action needs root. Re-run as root or install sudo."
  fi
  log "root permission required; re-running with sudo"
  exec sudo -E bash "$SELF_PATH" "$@"
}

detect_deploy_user() {
  DEPLOY_USER="${DEPLOY_USER:-${SUDO_USER:-$(id -un)}}"
  if [[ "$REPO_DIR" == /root/* && "$DEPLOY_USER" != "root" ]]; then
    warn "repo is under /root; using root as service user so systemd can read it"
    DEPLOY_USER="root"
  fi
  if ! id "$DEPLOY_USER" >/dev/null 2>&1; then
    die "DEPLOY_USER does not exist: $DEPLOY_USER"
  fi
  DEPLOY_GROUP="${DEPLOY_GROUP:-$(id -gn "$DEPLOY_USER")}"
  DEPLOY_HOME="$(getent passwd "$DEPLOY_USER" | cut -d: -f6)"
  if [[ -z "$DEPLOY_HOME" ]]; then
    DEPLOY_HOME="/root"
  fi
}

as_deploy_user() {
  if [[ "$DEPLOY_USER" == "root" ]]; then
    HOME="$DEPLOY_HOME" PATH="$PATH" "$@"
    return
  fi
  runuser -u "$DEPLOY_USER" -- env HOME="$DEPLOY_HOME" PATH="$PATH" "$@"
}

require_repo() {
  [[ -f "$REPO_DIR/package.json" ]] || die "package.json not found; run this script from the project root"
  [[ -d "$REPO_DIR/backend" ]] || die "backend directory not found; run this script from the project root"
}

version_ge() {
  [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

node_ok() {
  command -v node >/dev/null 2>&1 || return 1
  command -v npm >/dev/null 2>&1 || return 1
  local major
  major="$(node -v | sed -E 's/^v([0-9]+).*/\1/')"
  [[ "$major" =~ ^[0-9]+$ ]] && (( major >= 18 ))
}

go_ok() {
  command -v go >/dev/null 2>&1 || return 1
  local version
  version="$(go env GOVERSION 2>/dev/null || true)"
  if [[ -z "$version" ]]; then
    version="$(go version | awk '{print $3}')"
  fi
  version="${version#go}"
  version_ge "$version" "1.23"
}

apt_install() {
  [[ "$INSTALL_DEPS" == "1" ]] || die "missing dependencies and INSTALL_DEPS=0"
  command -v apt-get >/dev/null 2>&1 || die "automatic install currently supports Debian/Ubuntu with apt-get"
  export DEBIAN_FRONTEND=noninteractive
  log "installing base packages"
  apt-get update
  apt-get install -y ca-certificates curl git ffmpeg openssl iproute2 build-essential \
    python3 python3-requests python3-bs4 python3-lxml python3-socks
}

verify_crawler_python_deps() {
  command -v python3 >/dev/null 2>&1 || die "python3 is required for crawler scripts"
  python3 - <<'PY' || die "missing Python modules for crawler scripts: requests, bs4, lxml, socks"
import importlib.util
import sys

missing = [
    name
    for name in ("requests", "bs4", "lxml", "socks")
    if importlib.util.find_spec(name) is None
]
if missing:
    print("missing Python modules: " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
PY
}

install_node() {
  if node_ok; then
    log "Node $(node -v) and npm $(npm -v) are ready"
    return
  fi
  [[ "$INSTALL_DEPS" == "1" ]] || die "Node.js 18+ and npm are required"
  command -v apt-get >/dev/null 2>&1 || die "install Node.js 18+ manually, then re-run"
  log "installing Node.js 20 from NodeSource"
  curl -fsSL https://deb.nodesource.com/setup_20.x -o /tmp/video-site-nodesource.sh
  bash /tmp/video-site-nodesource.sh
  apt-get install -y nodejs
  node_ok || die "Node.js install finished, but node/npm version check still failed"
  log "Node $(node -v) and npm $(npm -v) are ready"
}

install_go() {
  if go_ok; then
    log "Go $(go env GOVERSION 2>/dev/null || go version | awk '{print $3}') is ready"
    return
  fi
  [[ "$INSTALL_DEPS" == "1" ]] || die "Go 1.23+ is required"

  local arch go_arch tmp url
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) go_arch="amd64" ;;
    aarch64|arm64) go_arch="arm64" ;;
    *) die "unsupported CPU architecture for automatic Go install: $arch" ;;
  esac

  url="https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz"
  tmp="$(mktemp -d)"
  log "installing Go ${GO_VERSION} from ${url}"
  curl -fL "$url" -o "$tmp/go.tgz"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp/go.tgz"
  rm -rf "$tmp"
  go_ok || die "Go install finished, but go version check still failed"
  log "Go $(go env GOVERSION) is ready"
}

install_dependencies() {
  if [[ "$INSTALL_DEPS" == "1" ]]; then
    apt_install
  fi
  install_node
  install_go
  command -v ffmpeg >/dev/null 2>&1 || die "ffmpeg is required"
  command -v ffprobe >/dev/null 2>&1 || die "ffprobe is required"
  verify_crawler_python_deps
}

ensure_ownership() {
  local paths=()
  [[ -e "$REPO_DIR/backend/config.yaml" ]] && paths+=("$REPO_DIR/backend/config.yaml")
  [[ -d "$REPO_DIR/backend/data" ]] && paths+=("$REPO_DIR/backend/data")
  [[ -d "$REPO_DIR/dist" ]] && paths+=("$REPO_DIR/dist")
  [[ -d "$REPO_DIR/node_modules" ]] && paths+=("$REPO_DIR/node_modules")
  [[ -e "$REPO_DIR/backend/server" ]] && paths+=("$REPO_DIR/backend/server")
  if (( ${#paths[@]} > 0 )); then
    chown -R "$DEPLOY_USER:$DEPLOY_GROUP" "${paths[@]}"
  fi
}

prepare_config() {
  local cfg="$REPO_DIR/backend/config.yaml"
  local example="$REPO_DIR/backend/config.example.yaml"
  mkdir -p "$REPO_DIR/backend/data"

  if [[ ! -f "$cfg" ]]; then
    log "creating backend/config.yaml from example"
    cp "$example" "$cfg"
    sed -i -E "s#listen: \".*\"#listen: \"$BACKEND_LISTEN\"#" "$cfg"
  else
    log "backend/config.yaml already exists; keeping it"
  fi

  if grep -q 'session_secret: "change-me-to-a-random-string"' "$cfg"; then
    local secret
    secret="$(openssl rand -hex 32)"
    sed -i -E "s#session_secret: \".*\"#session_secret: \"$secret\"#" "$cfg"
    log "generated a random session_secret"
  fi

  ensure_ownership
}

install_frontend() {
  log "installing frontend dependencies"
  if [[ -f "$REPO_DIR/package-lock.json" ]]; then
    as_deploy_user bash -lc "cd '$REPO_DIR' && npm ci"
  else
    as_deploy_user bash -lc "cd '$REPO_DIR' && npm install"
  fi

  log "building frontend"
  as_deploy_user bash -lc "cd '$REPO_DIR' && npm run build"
}

build_backend() {
  log "building backend binary"
  as_deploy_user bash -lc "cd '$REPO_DIR/backend' && go build -o server ./cmd/server"
}

systemd_env_lines() {
  local lines=""
  if [[ -n "${HTTP_PROXY:-}" ]]; then
    lines+="Environment=HTTP_PROXY=${HTTP_PROXY}"$'\n'
  fi
  if [[ -n "${HTTPS_PROXY:-}" ]]; then
    lines+="Environment=HTTPS_PROXY=${HTTPS_PROXY}"$'\n'
  fi
  if [[ -n "${NO_PROXY:-}" ]]; then
    lines+="Environment=NO_PROXY=${NO_PROXY}"$'\n'
  fi
  printf '%s' "$lines"
}

write_systemd_units() {
  local npm_bin backend_unit frontend_unit env_lines
  npm_bin="$(command -v npm)"
  backend_unit="/etc/systemd/system/${BACKEND_SERVICE}.service"
  frontend_unit="/etc/systemd/system/${FRONTEND_SERVICE}.service"
  env_lines="$(systemd_env_lines)"

  log "writing systemd unit: $backend_unit"
  cat >"$backend_unit" <<EOF
[Unit]
Description=Video Site Backend
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${DEPLOY_USER}
Group=${DEPLOY_GROUP}
WorkingDirectory=${REPO_DIR}/backend
ExecStart=${REPO_DIR}/backend/server
Restart=on-failure
RestartSec=5
TimeoutStopSec=20
Environment=HOME=${DEPLOY_HOME}
Environment=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
${env_lines}LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${BACKEND_SERVICE}

[Install]
WantedBy=multi-user.target
EOF

  log "writing systemd unit: $frontend_unit"
  cat >"$frontend_unit" <<EOF
[Unit]
Description=Video Site Frontend
After=network-online.target ${BACKEND_SERVICE}.service
Wants=network-online.target

[Service]
Type=simple
User=${DEPLOY_USER}
Group=${DEPLOY_GROUP}
WorkingDirectory=${REPO_DIR}
ExecStart=${npm_bin} run preview -- --host ${FRONTEND_HOST} --port ${FRONTEND_PORT}
Restart=on-failure
RestartSec=5
Environment=HOME=${DEPLOY_HOME}
Environment=NODE_ENV=production
Environment=PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${FRONTEND_SERVICE}

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "${BACKEND_SERVICE}.service" "${FRONTEND_SERVICE}.service" >/dev/null
}

open_firewall_port() {
  [[ "$CONFIGURE_UFW" == "1" ]] || return 0
  command -v ufw >/dev/null 2>&1 || return 0
  if ufw status 2>/dev/null | grep -qi "Status: active"; then
    log "UFW is active; allowing ${FRONTEND_PORT}/tcp"
    ufw allow "${FRONTEND_PORT}/tcp"
  fi
}

restart_services() {
  log "starting services"
  systemctl restart "${BACKEND_SERVICE}.service"
  systemctl restart "${FRONTEND_SERVICE}.service"
}

show_summary() {
  echo
  log "deployment finished"
  echo "  frontend: http://<server-ip>:${FRONTEND_PORT}/"
  echo "  admin:    http://<server-ip>:${FRONTEND_PORT}/admin"
  echo "  backend:  127.0.0.1:9192"
  echo
  echo "First visit will ask you to create the admin username and password."
  echo "Useful commands:"
  echo "  sudo bash deploy.sh status"
  echo "  sudo bash deploy.sh logs"
  echo "  sudo bash deploy.sh update"
}

show_status() {
  systemctl --no-pager --full status "${BACKEND_SERVICE}.service" "${FRONTEND_SERVICE}.service" || true
}

install_or_update() {
  local mode="$1"
  require_repo
  detect_deploy_user
  install_dependencies
  prepare_config
  install_frontend
  build_backend
  write_systemd_units
  open_firewall_port
  restart_services
  show_status
  if [[ "$mode" == "install" ]]; then
    show_summary
  fi
}

uninstall_services() {
  systemctl disable --now "${FRONTEND_SERVICE}.service" "${BACKEND_SERVICE}.service" 2>/dev/null || true
  rm -f "/etc/systemd/system/${FRONTEND_SERVICE}.service" "/etc/systemd/system/${BACKEND_SERVICE}.service"
  systemctl daemon-reload
  log "removed systemd services; repo, config, and data were kept"
}

main() {
  local action="${1:-install}"
  case "$action" in
    install|deploy)
      need_root "$@"
      install_or_update "install"
      ;;
    update)
      need_root "$@"
      install_or_update "update"
      ;;
    restart)
      need_root "$@"
      restart_services
      show_status
      ;;
    stop)
      need_root "$@"
      systemctl stop "${FRONTEND_SERVICE}.service" "${BACKEND_SERVICE}.service"
      ;;
    status)
      show_status
      ;;
    logs)
      journalctl -u "${BACKEND_SERVICE}.service" -u "${FRONTEND_SERVICE}.service" -f
      ;;
    uninstall)
      need_root "$@"
      uninstall_services
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
