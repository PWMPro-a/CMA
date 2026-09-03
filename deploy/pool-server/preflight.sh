#!/usr/bin/env bash
# Validate a customer CPA pool deployment without changing containers or data.
# This script is intentionally dependency-light so it can run before bootstrap.
set -euo pipefail

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)"
env_file="${CPA_POOL_ENV_FILE:-$script_dir/.env}"
compose_file="${CPA_POOL_COMPOSE_FILE:-$script_dir/compose.yml}"
dry_run="${CPA_POOL_DRY_RUN:-0}"
skip_docker="${CPA_POOL_SKIP_DOCKER:-0}"
allow_missing_secrets="${CPA_POOL_ALLOW_MISSING_SECRETS:-0}"
errors=0
warnings=0

usage() {
  cat <<USAGE
Usage: $(basename "$0") [options]

Options:
  --env-file PATH       Deployment environment file (default: ./\.env)
  --compose-file PATH   Compose file (default: ./compose.yml)
  --skip-docker         Skip Docker daemon and compose config checks
  --dry-run             Skip Docker checks (same as --skip-docker)
  --allow-missing-secrets  Do not fail when optional local secret files are absent
                           (storefront client secret is always required for shop666/p.666ttt.net)
  -h, --help            Show this help
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --env-file)
      [ "$#" -ge 2 ] || { echo "--env-file requires a path" >&2; exit 2; }
      env_file="$2"
      shift 2
      ;;
    --compose-file)
      [ "$#" -ge 2 ] || { echo "--compose-file requires a path" >&2; exit 2; }
      compose_file="$2"
      shift 2
      ;;
    --skip-docker)
      skip_docker=1
      shift
      ;;
    --dry-run)
      dry_run=1
      skip_docker=1
      shift
      ;;
    --allow-missing-secrets)
      allow_missing_secrets=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ "${env_file#/}" = "$env_file" ]; then env_file="$script_dir/$env_file"; fi
if [ "${compose_file#/}" = "$compose_file" ]; then compose_file="$script_dir/$compose_file"; fi

canonical_file_path() {
  local value="$1"
  local parent="$(dirname -- "$value")"
  local base="$(basename -- "$value")"
  if [ -d "$parent" ]; then
    printf '%s/%s' "$(CDPATH= cd -- "$parent" && pwd -P)" "$base"
  else
    # Keep a useful absolute path for a file that bootstrap will create later.
    case "$value" in
      /*) printf '%s' "$value" ;;
      *) printf '%s/%s' "$script_dir" "$value" ;;
    esac
  fi
}

env_file="$(canonical_file_path "$env_file")"
compose_file="$(canonical_file_path "$compose_file")"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  errors=$((errors + 1))
}
warn() {
  printf 'WARN: %s\n' "$*" >&2
  warnings=$((warnings + 1))
}
info() {
  printf 'INFO: %s\n' "$*"
}

# Read KEY=VALUE without sourcing .env. This keeps shell syntax in a customer
# file from being executed by the preflight process.
env_get() {
  local key="$1"
  [ -f "$env_file" ] || return 1
  awk -v wanted="$key" '
    /^[[:space:]]*#/ || index($0, "=") == 0 { next }
    {
      k = $0
      sub(/=.*/, "", k)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", k)
      if (k != wanted) next
      v = $0
      sub(/^[^=]*=/, "", v)
      sub(/\r$/, "", v)
      if (length(v) >= 2 && substr(v, 1, 1) == "\"" && substr(v, length(v), 1) == "\"") {
        v = substr(v, 2, length(v) - 2)
      }
      print v
      exit
    }
  ' "$env_file"
}

value_or() {
  local value=""
  value="$(env_get "$1" 2>/dev/null || true)"
  if [ -n "$value" ]; then
    printf '%s' "$value"
  else
    printf '%s' "${2:-}"
  fi
}

is_placeholder() {
  local value="${1:-}"
  local lower=""
  lower="$(printf '%s' "$value" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
  [ -z "$lower" ] && return 0
  case "$lower" in
    replace-with*|changeme*|change-me*|set-me*|your-*|'<*>') return 0 ;;
    *) return 1 ;;
  esac
}

is_blank_or_placeholder_secret() {
  local value="${1:-}"
  if [ -z "$value" ] || [[ "$value" =~ ^[[:space:]]*$ ]]; then
    return 0
  fi
  is_placeholder "$value"
}

storefront_secret_required() {
  local provider=""
  local api_base=""
  local authority=""
  local host=""

  provider="$(value_or CPA_LICENSE_PROVIDER shop666 | LC_ALL=C tr '[:upper:]' '[:lower:]')"
  provider="${provider//[[:space:]]/}"
  [ "$provider" = "shop666" ] && return 0

  api_base="$(value_or CPA_LICENSE_API_BASE_URL https://p.666ttt.net/api/storefront)"
  api_base="$(printf '%s' "$api_base" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
  case "$api_base" in
    http://*|https://*) authority="${api_base#*://}" ;;
    *) return 1 ;;
  esac
  authority="${authority%%/*}"
  authority="${authority##*@}"
  host="${authority%%:*}"
  [ "$host" = "p.666ttt.net" ]
}

check_nonempty() {
  local name="$1"
  local value="${2:-}"
  if [ -z "$value" ] || is_placeholder "$value"; then
    fail "$name must be set to a non-placeholder value in $env_file"
  fi
}

check_single_line() {
  local name="$1"
  local value="${2:-}"
  case "$value" in
    *$'\n'*|*$'\r'*) fail "$name must be a single line" ;;
  esac
}

check_port() {
  local name="$1"
  local value="${2:-}"
  if ! [[ "$value" =~ ^[0-9]+$ ]]; then
    fail "$name must be an integer between 1 and 65535 (got $value)"
    return
  fi
  if ! awk -v p="$value" 'BEGIN { exit !(p >= 1 && p <= 65535) }'; then
    fail "$name must be an integer between 1 and 65535 (got $value)"
  fi
}

check_name() {
  local name="$1"
  local value="${2:-}"
  if ! [[ "$value" =~ ^[a-z0-9][a-z0-9_-]*$ ]]; then
    fail "$name must match [a-z0-9][a-z0-9_-]* (got $value)"
  fi
}

check_image() {
  local name="$1"
  local value="${2:-}"
  check_nonempty "$name" "$value"
  if [[ "$value" == *[!A-Za-z0-9._/@:-]* ]]; then
    fail "$name contains unsupported characters"
  fi
}

check_url() {
  local name="$1"
  local value="${2:-}"
  check_nonempty "$name" "$value"
  case "$value" in
    http://*|https://*) ;;
    *) fail "$name must start with http:// or https://" ;;
  esac
  if [[ "$value" == *$'\n'* || "$value" == *$'\r'* || "$value" == *'#'* ||
        "$value" == *'"'* || "$value" == *"'"* || "$value" == *'\\'* ||
        "$value" == *'$'* || "$value" == *'`'* ]]; then
    fail "$name contains unsupported characters"
  fi
}

check_path() {
  local name="$1"
  local value="${2:-}"
  check_nonempty "$name" "$value"
  case "$value" in
    /*|./*|../*) ;;
    *) fail "$name must be an absolute path or a ./ relative path (got $value)" ;;
  esac
  if [[ "$value" == *$'\n'* || "$value" == *$'\r'* || "$value" == *'#'* ||
        "$value" == *'"'* || "$value" == *"'"* || "$value" == *'$'* ||
        "$value" == *'`'* ]]; then
    fail "$name contains unsupported characters"
  fi
}

check_duration() {
  local name="$1"
  local value="${2:-}"
  check_nonempty "$name" "$value"
  if [[ "$value" == 0 || "$value" == 0s || "$value" == 0m || "$value" == 0h || "$value" == 0d ]]; then
    fail "$name must be greater than zero"
  elif ! [[ "$value" =~ ^[0-9]+(ns|us|ms|s|m|h|d)([0-9]+(ns|us|ms|s|m|h|d))*$ ]]; then
    fail "$name must use a duration such as 6h or 10m (got $value)"
  fi
}

check_public_key() {
  local name="$1"
  local value="${2:-}"
  check_nonempty "$name" "$value"
  if [[ "$value" =~ ^[A-Fa-f0-9]{64}$ ]]; then
    return
  fi
  if ! [[ "$value" =~ ^[A-Za-z0-9+/_-]{43}={0,1}$ ]]; then
    fail "$name must be a 32-byte Ed25519 key encoded as base64/base64url or hex"
    return
  fi
  # Python is optional; when present, verify the decoded byte length rather
  # than accepting a merely well-shaped string.
  if command -v python3 >/dev/null 2>&1; then
    if ! python3 - "$value" <<'PY' >/dev/null 2>&1
import base64
import sys
try:
    value = sys.argv[1]
    decoded = base64.urlsafe_b64decode(value + "=" * ((4 - len(value) % 4) % 4))
except Exception:
    raise SystemExit(1)
raise SystemExit(0 if len(decoded) == 32 else 1)
PY
    then
      fail "$name does not decode to a 32-byte Ed25519 public key"
    fi
  fi
}

check_secret_file() {
  local name="$1"
  local value="$2"
  local resolved="$value"
  [ -n "$resolved" ] || return 0
  if [ "${resolved#/}" = "$resolved" ]; then
    resolved="$script_dir/${resolved#./}"
  fi
  if [ ! -f "$resolved" ]; then
    if [ "$allow_missing_secrets" = "1" ]; then
      warn "$name file is not present yet (bootstrap will create it): $resolved"
    else
      fail "$name points to a missing file: $resolved"
    fi
  elif [ ! -r "$resolved" ]; then
    fail "$name points to an unreadable file: $resolved"
  fi
  if [ -f "$resolved" ]; then
    local mode=""
    if mode="$(stat -c '%a' "$resolved" 2>/dev/null)"; then :; elif mode="$(stat -f '%Lp' "$resolved" 2>/dev/null)"; then :; else mode=""; fi
    if [ -n "$mode" ] && [ "$mode" -gt 600 ] 2>/dev/null; then
      warn "$name file is readable by group/other; chmod 600 is recommended: $resolved"
    fi
  fi
}

check_storefront_secret_file() {
  local name="$1"
  local value="$2"
  local resolved="$value"
  local contents=""
  local mode=""

  if [ -z "$resolved" ] || is_placeholder "$resolved" || [ "$resolved" = "/dev/null" ]; then
    fail "$name is required for the configured storefront; set it to a host-readable file containing the storefront-issued secret (the value is never printed)"
    return
  fi
  if [ "${resolved#/}" = "$resolved" ]; then
    resolved="$script_dir/${resolved#./}"
  fi
  if [ ! -f "$resolved" ]; then
    fail "$name must point to a readable storefront secret file before CPA can start: $resolved (inject the matching secret and rerun bootstrap)"
    return
  fi
  if [ ! -r "$resolved" ]; then
    fail "$name points to an unreadable storefront secret file: $resolved"
    return
  fi
  if ! contents="$(cat "$resolved" 2>/dev/null)"; then
    fail "$name could not be read: $resolved"
    return
  fi
  contents="${contents%$'\r'}"
  if [ -z "$contents" ] || [[ "$contents" =~ ^[[:space:]]*$ ]] || is_placeholder "$contents"; then
    fail "$name must contain the matching storefront-issued secret (one line); an empty or generated placeholder is not accepted"
  elif [[ "$contents" == *$'\n'* || "$contents" == *$'\r'* ]]; then
    fail "$name must contain a single-line storefront secret: $resolved"
  fi
  if mode="$(stat -c '%a' "$resolved" 2>/dev/null)"; then :; elif mode="$(stat -f '%Lp' "$resolved" 2>/dev/null)"; then :; else mode=""; fi
  if [ -n "$mode" ] && [ "$mode" != "600" ]; then
    fail "$name must use mode 600; run chmod 600 '$resolved' before starting CPA"
  fi
}

check_listener() {
  local name="$1"
  local port="$2"
  if command -v lsof >/dev/null 2>&1; then
    if lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | grep -q .; then
      warn "$name port $port already has a listener; verify it belongs to this stack before starting"
    fi
  elif command -v ss >/dev/null 2>&1; then
    if ss -ltn 2>/dev/null | awk -v p=":$port" '$4 ~ p"$" {found=1} END {exit !found}'; then
      warn "$name port $port already has a listener; verify it belongs to this stack before starting"
    fi
  fi
}

if [ ! -f "$env_file" ]; then
  fail "environment file is missing: $env_file (run bootstrap.sh first)"
fi
if [ ! -f "$compose_file" ]; then
  fail "compose file is missing: $compose_file"
fi
if [ "$errors" -gt 0 ]; then
  printf 'Preflight blocked: %s error(s), %s warning(s).\n' "$errors" "$warnings" >&2
  exit 1
fi

compose_project="$(value_or COMPOSE_PROJECT_NAME cpamp-cpa)"
cpa_image="$(value_or CPA_IMAGE '')"
cpamp_image="$(value_or CPAMP_IMAGE '')"
cpa_port="$(value_or CPA_PORT 8317)"
cpamp_port="$(value_or CPAMP_PORT 18317)"
cpamp_internal_port="$(value_or CPAMP_INTERNAL_PORT 18317)"
agent_port="$(value_or CPAMP_AGENT_PORT 18417)"
cpa_name="$(value_or CPA_CONTAINER_NAME cli-proxy-api)"
cpamp_name="$(value_or CPAMP_CONTAINER_NAME cpa-manager-plus)"
agent_name="$(value_or CPAMP_AGENT_CONTAINER_NAME cpamp-agent)"
network_name="$(value_or CPAMP_NETWORK_NAME cpamp-cpa_default)"
cpa_data_dir="$(value_or CPA_DATA_DIR ./data/cpa)"
cpamp_data_dir="$(value_or CPAMP_DATA_DIR ./data/manager)"
stack_root="$(value_or CPAMP_STACK_ROOT .)"
backup_root="$(value_or CPAMP_BACKUP_ROOT ./backups)"
admin_key_file="$(value_or CPAMP_ADMIN_KEY_FILE ./secrets/cpamp-admin-key)"
management_key_file="$(value_or CPA_MANAGEMENT_KEY_FILE ./secrets/cpa-management-key)"
license_secret_file="$(value_or CPA_LICENSE_CLIENT_SECRET_HOST_PATH '')"
if [ -z "$license_secret_file" ]; then
  license_secret_file="$(value_or CPA_LICENSE_CLIENT_SECRET_FILE '')"
fi
if [ -z "$license_secret_file" ]; then
  # Compose falls back to /dev/null for key-only or legacy direct-secret
  # deployments. An explicitly configured host path is validated below.
  license_secret_file="/dev/null"
fi

check_name COMPOSE_PROJECT_NAME "$compose_project"
check_name CPAMP_NETWORK_NAME "$network_name"
check_image CPA_IMAGE "$cpa_image"
check_image CPAMP_IMAGE "$cpamp_image"
if [ "${CPA_POOL_ALLOW_LATEST:-0}" != "1" ]; then
  case "$cpa_image" in
    latest|*:latest) fail "CPA_IMAGE must use a pinned tag or digest; set CPA_POOL_ALLOW_LATEST=1 only for an intentional floating test" ;;
  esac
  case "$cpamp_image" in
    latest|*:latest) fail "CPAMP_IMAGE must use a pinned tag or digest; set CPA_POOL_ALLOW_LATEST=1 only for an intentional floating test" ;;
  esac
fi
check_port CPA_PORT "$cpa_port"
check_port CPAMP_PORT "$cpamp_port"
check_port CPAMP_INTERNAL_PORT "$cpamp_internal_port"
check_port CPAMP_AGENT_PORT "$agent_port"
if [ "$cpa_port" = "$cpamp_port" ] || [ "$cpa_port" = "$agent_port" ] || [ "$cpamp_port" = "$agent_port" ]; then
  fail "CPA_PORT, CPAMP_PORT, and CPAMP_AGENT_PORT must be distinct for host-network services"
fi
check_name CPA_CONTAINER_NAME "$cpa_name"
check_name CPAMP_CONTAINER_NAME "$cpamp_name"
check_name CPAMP_AGENT_CONTAINER_NAME "$agent_name"
if [ "$cpa_name" = "$cpamp_name" ] || [ "$cpa_name" = "$agent_name" ] || [ "$cpamp_name" = "$agent_name" ]; then
  fail "CPA/CPAMP/Agent container names must be distinct"
fi
if [ "$cpa_name" != "cli-proxy-api" ] || [ "$cpamp_name" != "cpa-manager-plus" ] || [ "$agent_name" != "cpamp-agent" ]; then
  warn "Non-standard container names are supported for isolated Compose runs; CPAMP container-operations upgrade/network actions target the standard names"
fi
if [ "$network_name" != "cpamp-cpa_default" ]; then
  warn "Non-standard network name is supported for isolated Compose runs; CPAMP container-operations actions target cpamp-cpa_default"
fi
check_path CPA_DATA_DIR "$cpa_data_dir"
check_path CPAMP_DATA_DIR "$cpamp_data_dir"
check_path CPAMP_STACK_ROOT "$stack_root"
check_path CPAMP_BACKUP_ROOT "$backup_root"
check_path CPAMP_ADMIN_KEY_FILE "$admin_key_file"
check_path CPA_MANAGEMENT_KEY_FILE "$management_key_file"
if [ "$license_secret_file" != "/dev/null" ]; then
  check_path CPA_LICENSE_CLIENT_SECRET_HOST_PATH "$license_secret_file"
fi

check_nonempty CPAMP_AGENT_TOKEN "$(value_or CPAMP_AGENT_TOKEN '')"
check_nonempty CPA_MANAGEMENT_KEY "$(value_or CPA_MANAGEMENT_KEY '')"
check_nonempty CPA_MANAGER_ADMIN_KEY "$(value_or CPA_MANAGER_ADMIN_KEY '')"
for single_line_key in COMPOSE_PROJECT_NAME CPA_IMAGE CPAMP_IMAGE CPA_MANAGEMENT_KEY CPA_MANAGER_ADMIN_KEY CPAMP_AGENT_TOKEN CPA_LICENSE_PUBLIC_KEY CPA_LICENSE_CLIENT_ID CPA_LICENSE_API_BASE_URL CPA_LICENSE_SHOP_AUTH_URL; do
  check_single_line "$single_line_key" "$(value_or "$single_line_key" '')"
done
check_public_key CPA_LICENSE_PUBLIC_KEY "$(value_or CPA_LICENSE_PUBLIC_KEY '')"
if plugin_key="$(value_or CPA_LICENSE_PLUGIN_PUBLIC_KEY '')"; then
  if [ -n "$plugin_key" ]; then check_public_key CPA_LICENSE_PLUGIN_PUBLIC_KEY "$plugin_key"; fi
fi
check_url CPA_LICENSE_API_BASE_URL "$(value_or CPA_LICENSE_API_BASE_URL https://p.666ttt.net/api/storefront)"
check_url CPA_LICENSE_SHOP_AUTH_URL "$(value_or CPA_LICENSE_SHOP_AUTH_URL 'https://p.666ttt.net/shop/?authorize=cpa')"
check_duration CPA_LICENSE_REFRESH_INTERVAL "$(value_or CPA_LICENSE_REFRESH_INTERVAL 10m)"
check_duration CPA_LICENSE_GRACE_PERIOD "$(value_or CPA_LICENSE_GRACE_PERIOD 6h)"
for license_path_key in CPA_LICENSE_SHOP_EXCHANGE_PATH CPA_LICENSE_ACTIVATE_PATH CPA_LICENSE_REFRESH_PATH CPA_LICENSE_VERIFY_PATH CPA_LICENSE_GRACE_PATH CPA_LICENSE_CLAIM_PATH; do
  path_default=""
  case "$license_path_key" in
    CPA_LICENSE_SHOP_EXCHANGE_PATH) path_default="/licenses/exchange" ;;
    CPA_LICENSE_ACTIVATE_PATH) path_default="/licenses/activate" ;;
    CPA_LICENSE_REFRESH_PATH) path_default="/licenses/refresh" ;;
    CPA_LICENSE_VERIFY_PATH) path_default="/licenses/verify" ;;
    CPA_LICENSE_GRACE_PATH) path_default="/licenses/grace" ;;
  esac
  path_value="$(value_or "$license_path_key" "$path_default")"
  [ -n "$path_value" ] || { [ "$license_path_key" = CPA_LICENSE_CLAIM_PATH ] && continue; }
  case "$path_value" in
    /*) ;;
    *) fail "$license_path_key must start with / (got $path_value)" ;;
  esac
done
check_secret_file CPAMP_ADMIN_KEY_FILE "$admin_key_file"
check_secret_file CPA_MANAGEMENT_KEY_FILE "$management_key_file"
if storefront_secret_required; then
  check_storefront_secret_file CPA_LICENSE_CLIENT_SECRET_HOST_PATH "$license_secret_file"
elif [ "$license_secret_file" != "/dev/null" ]; then
  check_secret_file CPA_LICENSE_CLIENT_SECRET_HOST_PATH "$license_secret_file"
fi

check_listener CPA "$cpa_port"
check_listener CPAMP "$cpamp_port"
check_listener CPAMP_AGENT "$agent_port"

if [ "$skip_docker" != "1" ] && [ "$dry_run" != "1" ]; then
  if ! command -v docker >/dev/null 2>&1; then
    fail "docker command is required (or use --skip-docker for a file-only check)"
  else
    if ! docker compose --env-file "$env_file" -f "$compose_file" version >/dev/null 2>&1; then
      fail "docker compose plugin is required"
    fi
    if ! docker info >/dev/null 2>&1; then
      fail "Docker daemon is not available"
    fi
    if [ "$errors" -eq 0 ] && ! docker compose --env-file "$env_file" -f "$compose_file" config --quiet >/dev/null 2>&1; then
      fail "docker compose config failed; inspect the rendered interpolation with: docker compose --env-file '$env_file' -f '$compose_file' config"
    fi
  fi
else
  info "Docker checks skipped"
fi

if [ "$errors" -gt 0 ]; then
  printf 'Preflight blocked: %s error(s), %s warning(s).\n' "$errors" "$warnings" >&2
  exit 1
fi
printf 'Preflight passed: %s warning(s).\n' "$warnings"
printf '  project=%s CPA=%s:%s CPAMP=%s:%s agent=%s:%s\n' \
  "$compose_project" "$cpa_image" "$cpa_port" "$cpamp_image" "$cpamp_port" "$agent_name" "$agent_port"
printf '  data=%s manager-data=%s stack=%s backups=%s\n' \
  "$cpa_data_dir" "$cpamp_data_dir" "$stack_root" "$backup_root"
