#!/usr/bin/env bash
# Bootstrap and operate the public CPA pool stack.
#
# The script is deliberately local and deterministic:
#   * .env is created once and then preserved between runs;
#   * generated secrets are stored with mode 600;
#   * an existing CPA config and license state are never overwritten by default;
#   * every Docker Compose invocation names both --env-file and -f explicitly.
set -euo pipefail

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)"
project_dir="$script_dir"
env_file=""
compose_file=""
action="start"
force_config="${CPA_POOL_FORCE_CONFIG:-0}"
dry_run="${CPA_POOL_DRY_RUN:-0}"
no_pull="${CPA_POOL_NO_PULL:-0}"
no_start="${CPA_POOL_NO_START:-0}"
health_timeout="${CPA_POOL_HEALTH_TIMEOUT:-120}"
tmp_env=""

usage() {
  cat <<USAGE
Usage: $(basename "$0") [options]

Actions (default: --start):
  --start                Initialize, preflight, pull, and start the stack
  --pull                 Initialize, preflight, and pull images only
  --render              Initialize files and run file-only preflight
  --down                Stop and remove this compose project's containers
  --status              Show compose status and health

Options:
  --dir PATH             Stack directory (default: directory of this script)
  --env-file PATH        Environment file (default: <dir>/.env)
  --compose-file PATH    Compose file (default: <dir>/compose.yml)
  --force-config         Back up and regenerate config.yaml
  --no-pull              Skip an explicit image pull on --start
  --no-start             Render/preflight only on --start
  --dry-run              Print the plan without writing files or touching Docker
  -h, --help             Show this help

Environment:
  CPA_POOL_DRY_RUN=1, CPA_POOL_FORCE_CONFIG=1, CPA_POOL_NO_PULL=1,
  CPA_POOL_NO_START=1, CPA_POOL_HEALTH_TIMEOUT=120
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --start) action="start"; shift ;;
    --pull) action="pull"; shift ;;
    --render|--init) action="render"; shift ;;
    --down) action="down"; shift ;;
    --status) action="status"; shift ;;
    --dir|--project-dir)
      [ "$#" -ge 2 ] || { echo "$1 requires a path" >&2; exit 2; }
      project_dir="$2"
      shift 2
      ;;
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
    --force-config) force_config=1; shift ;;
    --no-pull) no_pull=1; shift ;;
    --no-start) no_start=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "${project_dir#/}" = "$project_dir" ]; then
  project_dir="$script_dir/$project_dir"
fi
if [ ! -d "$project_dir" ]; then
  if [ "$dry_run" = "1" ]; then
    project_dir="$(CDPATH= cd -- "$(dirname -- "$project_dir")" && pwd -P)/$(basename -- "$project_dir")"
  else
    mkdir -p "$project_dir"
  fi
fi
if [ -d "$project_dir" ]; then
  project_dir="$(CDPATH= cd -- "$project_dir" && pwd -P)"
fi
if [ -z "$env_file" ]; then env_file="$project_dir/.env"; elif [ "${env_file#/}" = "$env_file" ]; then env_file="$project_dir/$env_file"; fi
if [ -z "$compose_file" ]; then compose_file="$project_dir/compose.yml"; elif [ "${compose_file#/}" = "$compose_file" ]; then compose_file="$project_dir/$compose_file"; fi
if [ -e "$env_file" ]; then env_file="$(CDPATH= cd -- "$(dirname -- "$env_file")" && pwd -P)/$(basename -- "$env_file")"; else env_file="$project_dir/$(basename -- "$env_file")"; fi
if [ -e "$compose_file" ]; then compose_file="$(CDPATH= cd -- "$(dirname -- "$compose_file")" && pwd -P)/$(basename -- "$compose_file")"; else compose_file="$project_dir/$(basename -- "$compose_file")"; fi

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
info() { printf 'INFO: %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
command_exists() { command -v "$1" >/dev/null 2>&1; }

case "$health_timeout" in
  ''|*[!0-9]*) die "CPA_POOL_HEALTH_TIMEOUT must be a positive integer" ;;
  0) die "CPA_POOL_HEALTH_TIMEOUT must be greater than zero" ;;
esac

# Read KEY=VALUE without sourcing the file. Values are intentionally kept
# simple by preflight, and shell code in .env is never evaluated.
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

set_env() {
  local key="$1"
  local value="$2"
  local tmp="${env_file}.tmp.$$"
  [ -f "$env_file" ] || : > "$env_file"
  awk -v wanted="$key" -v replacement="$value" '
    BEGIN { updated = 0 }
    /^[[:space:]]*#/ || index($0, "=") == 0 { print; next }
    {
      k = $0
      sub(/=.*/, "", k)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", k)
      if (k == wanted) {
        if (!updated) { print wanted "=" replacement; updated = 1 }
        next
      }
      print
    }
    END { if (!updated) print wanted "=" replacement }
  ' "$env_file" > "$tmp"
  mv -f "$tmp" "$env_file"
  chmod 600 "$env_file" 2>/dev/null || true
}

value_or() {
  local value=""
  value="$(env_get "$1" 2>/dev/null || true)"
  if [ -n "$value" ]; then printf '%s' "$value"; else printf '%s' "${2:-}"; fi
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
  # A storefront secret is opaque, but it must contain at least one
  # non-whitespace character and must not be one of the template markers.
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
  # URLs accepted by preflight cannot contain userinfo. Strip an optional
  # port so the production host remains protected when it is non-default.
  authority="${authority##*@}"
  host="${authority%%:*}"
  [ "$host" = "p.666ttt.net" ]
}

random_alnum() {
  local length="${1:-32}"
  local value=""
  local chunk=""
  while [ "${#value}" -lt "$length" ]; do
    if command_exists openssl; then
      chunk="$(openssl rand -base64 96 | LC_ALL=C tr -dc 'A-Za-z0-9')"
    elif [ -r /dev/urandom ]; then
      chunk="$(dd if=/dev/urandom bs=256 count=1 2>/dev/null | LC_ALL=C tr -dc 'A-Za-z0-9')"
    else
      die "openssl or /dev/urandom is required to generate deployment secrets"
    fi
    value="${value}${chunk}"
  done
  printf '%s' "${value:0:$length}"
}

trim_file_value() {
  local file="$1"
  local value=""
  [ -f "$file" ] || return 1
  value="$(cat "$file")"
  value="${value%$'\r'}"
  value="${value%$'\n'}"
  case "$value" in
    *$'\n'*|*$'\r'*) return 1 ;;
  esac
  [ -n "$value" ] || return 1
  printf '%s' "$value"
}

resolve_path() {
  local value="$1"
  case "$value" in
    /*) printf '%s' "$value" ;;
    .) printf '%s' "$project_dir" ;;
    ./*) printf '%s/%s' "$project_dir" "${value#./}" ;;
    *) printf '%s/%s' "$project_dir" "$value" ;;
  esac
}

normalize_path_var() {
  local key="$1"
  local default_value="$2"
  local value=""
  value="$(value_or "$key" "$default_value")"
  [ -n "$value" ] || value="$default_value"
  value="$(resolve_path "$value")"
  set_env "$key" "$value"
  printf '%s' "$value"
}

ensure_file_secret() {
  local value_key="$1"
  local path_key="$2"
  local default_path="$3"
  local prefix="$4"
  local path=""
  local value=""
  local existing=""
  path="$(normalize_path_var "$path_key" "$default_path")"
  if [ "$dry_run" != "1" ]; then mkdir -p "$(dirname -- "$path")"; fi
  if existing="$(trim_file_value "$path" 2>/dev/null)"; then
    value="$existing"
  else
    value="$(value_or "$value_key" '')"
    if [ -z "$value" ] || is_placeholder "$value"; then
      value="${prefix}_$(random_alnum 32)"
    fi
    case "$value" in
      *$'\n'*|*$'\r'*) die "$value_key must be a single-line secret" ;;
    esac
    if [ "$dry_run" != "1" ]; then
      printf '%s\n' "$value" > "$path"
      chmod 600 "$path"
    fi
  fi
  set_env "$value_key" "$value"
  set_env "$path_key" "$path"
  printf '%s' "$value"
}

ensure_env_secret() {
  local key="$1"
  local prefix="$2"
  local value=""
  value="$(value_or "$key" '')"
  if [ -z "$value" ] || is_placeholder "$value"; then value="${prefix}_$(random_alnum 32)"; fi
  case "$value" in *$'\n'*|*$'\r'*) die "$key must be a single-line secret" ;; esac
  set_env "$key" "$value"
  printf '%s' "$value"
}

backup_existing_config() {
  local config_file="$1"
  local backup_dir="$project_dir/backups"
  local backup_file=""
  [ -f "$config_file" ] || return 0
  mkdir -p "$backup_dir"
  backup_file="$backup_dir/config-$(date '+%Y%m%d-%H%M%S').yaml"
  cp -p "$config_file" "$backup_file"
  info "Existing CPA config backed up to $backup_file"
}

sed_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//&/\\&}"
  value="${value//|/\\|}"
  printf '%s' "$value"
}

render_config() {
  local config_file="$1"
  local template="$script_dir/config.yaml.template"
  local tmp="${config_file}.tmp.$$"
  local cpa_port="$(value_or CPA_PORT 8317)"
  local management_key="$(value_or CPA_MANAGEMENT_KEY '')"
  local demo_key="$(value_or CPA_DEMO_API_KEY '')"
  local provider="$(value_or CPA_LICENSE_PROVIDER shop666)"
  local product="$(value_or CPA_LICENSE_PRODUCT_CODE CPA)"
  local api_base="$(value_or CPA_LICENSE_API_BASE_URL https://p.666ttt.net/api/storefront)"
  local public_key="$(value_or CPA_LICENSE_PUBLIC_KEY '')"
  local plugin_key="$(value_or CPA_LICENSE_PLUGIN_PUBLIC_KEY '')"
  local client_id="$(value_or CPA_LICENSE_CLIENT_ID '')"
  local state_dir="$(value_or CPA_LICENSE_STATE_DIR /CLIProxyAPI/data/license)"
  local shop_auth="$(value_or CPA_LICENSE_SHOP_AUTH_URL 'https://p.666ttt.net/shop/?authorize=cpa')"
  local exchange_path="$(value_or CPA_LICENSE_SHOP_EXCHANGE_PATH /licenses/exchange)"
  local activate_path="$(value_or CPA_LICENSE_ACTIVATE_PATH /licenses/activate)"
  local refresh_path="$(value_or CPA_LICENSE_REFRESH_PATH /licenses/refresh)"
  local verify_path="$(value_or CPA_LICENSE_VERIFY_PATH /licenses/verify)"
  local grace_path="$(value_or CPA_LICENSE_GRACE_PATH /licenses/grace)"
  local refresh_interval="$(value_or CPA_LICENSE_REFRESH_INTERVAL 10m)"
  local grace_period="$(value_or CPA_LICENSE_GRACE_PERIOD 6h)"
  local storage_key="$(value_or CPA_LICENSE_STORAGE_KEY '')"
  local executable_sha="$(value_or CPA_LICENSE_EXECUTABLE_SHA256 '')"
  local claim_path="$(value_or CPA_LICENSE_CLAIM_PATH '')"
  [ -f "$template" ] || die "Missing config template: $template"
  [ -n "$management_key" ] || die "CPA_MANAGEMENT_KEY is required before rendering config"
  [ -n "$demo_key" ] || die "CPA_DEMO_API_KEY is required before rendering config"
  if [ "$dry_run" = "1" ]; then
    info "Would render $config_file from $template (existing config is preserved)"
    return 0
  fi
  mkdir -p "$(dirname -- "$config_file")"
  if [ -f "$config_file" ] && [ "$force_config" != "1" ]; then
    info "Existing CPA config preserved: $config_file"
    return 0
  fi
  if [ -f "$config_file" ]; then backup_existing_config "$config_file"; fi
  cp "$template" "$tmp"
  for pair in \
    "__CPA_PORT__|$cpa_port" \
    "__CPA_MANAGEMENT_KEY__|$management_key" \
    "__CPA_DEMO_API_KEY__|$demo_key" \
    "__CPA_LICENSE_PROVIDER__|$provider" \
    "__CPA_LICENSE_PRODUCT_CODE__|$product" \
    "__CPA_LICENSE_API_BASE_URL__|$api_base" \
    "__CPA_LICENSE_PUBLIC_KEY__|$public_key" \
    "__CPA_LICENSE_PLUGIN_PUBLIC_KEY__|$plugin_key" \
    "__CPA_LICENSE_CLIENT_ID__|$client_id" \
    "__CPA_LICENSE_STATE_DIR__|$state_dir" \
    "__CPA_LICENSE_SHOP_AUTH_URL__|$shop_auth" \
    "__CPA_LICENSE_SHOP_EXCHANGE_PATH__|$exchange_path" \
    "__CPA_LICENSE_ACTIVATE_PATH__|$activate_path" \
    "__CPA_LICENSE_REFRESH_PATH__|$refresh_path" \
    "__CPA_LICENSE_VERIFY_PATH__|$verify_path" \
    "__CPA_LICENSE_GRACE_PATH__|$grace_path" \
    "__CPA_LICENSE_REFRESH_INTERVAL__|$refresh_interval" \
    "__CPA_LICENSE_GRACE_PERIOD__|$grace_period" \
    "__CPA_LICENSE_STORAGE_KEY__|$storage_key" \
    "__CPA_LICENSE_EXECUTABLE_SHA256__|$executable_sha" \
    "__CPA_LICENSE_CLAIM_PATH__|$claim_path"; do
    placeholder="${pair%%|*}"
    replacement="${pair#*|}"
    escaped="$(sed_escape "$replacement")"
    sed "s|$placeholder|$escaped|g" "$tmp" > "${tmp}.next"
    mv -f "${tmp}.next" "$tmp"
  done
  if grep -q '__CPA_[A-Z0-9_]*__' "$tmp"; then
    rm -f "$tmp"
    die "Unresolved placeholder remains in generated CPA config"
  fi
  mv -f "$tmp" "$config_file"
  chmod 600 "$config_file"
  info "Rendered CPA config: $config_file"
}

copy_env_template() {
  local example="$script_dir/.env.example"
  [ -f "$example" ] || die "Missing environment template: $example"
  if [ ! -f "$env_file" ]; then
    if [ "$dry_run" = "1" ]; then
      tmp_env="$(mktemp "${TMPDIR:-/tmp}/cpamp-pool-env.XXXXXX")"
      cp "$example" "$tmp_env"
      env_file="$tmp_env"
      info "Would create $project_dir/.env from $example"
    else
      cp "$example" "$env_file"
      chmod 600 "$env_file"
      info "Created $env_file from $example"
    fi
  fi
}

copy_compose_template() {
  local template="$script_dir/compose.yml"
  [ -f "$template" ] || die "Missing compose template: $template"
  if [ -f "$compose_file" ]; then return 0; fi
  if [ "$dry_run" = "1" ]; then
    info "Would copy $template to $compose_file"
  else
    mkdir -p "$(dirname -- "$compose_file")"
    cp "$template" "$compose_file"
    chmod 644 "$compose_file" 2>/dev/null || true
    info "Created $compose_file from $template"
  fi
}

copy_support_files() {
  local name=""
  local source=""
  local target=""
  for name in bootstrap.sh preflight.sh config.yaml.template .gitignore; do
    source="$script_dir/$name"
    target="$project_dir/$name"
    [ -f "$source" ] || continue
    [ "$source" = "$target" ] && continue
    if [ -e "$target" ]; then continue; fi
    if [ "$dry_run" = "1" ]; then
      info "Would copy $source to $target"
    else
      cp "$source" "$target"
      case "$name" in
        *.sh) chmod 755 "$target" ;;
        *) chmod 644 "$target" 2>/dev/null || true ;;
      esac
    fi
  done
}

cleanup() {
  if [ -n "$tmp_env" ] && [ -f "$tmp_env" ]; then rm -f "$tmp_env"; fi
}
trap cleanup EXIT

# Operations that do not initialize files first.
if [ "$action" = "down" ] || [ "$action" = "status" ]; then
  [ -f "$env_file" ] || die "Missing $env_file; run bootstrap.sh --render first"
  [ -f "$compose_file" ] || die "Missing $compose_file"
  if [ "$dry_run" = "1" ]; then
    info "Would run: docker compose --env-file '$env_file' -f '$compose_file' $action"
    exit 0
  fi
  command_exists docker || die "docker command is required"
  if [ "$action" = "down" ]; then
    docker compose --env-file "$env_file" -f "$compose_file" down
  else
    docker compose --env-file "$env_file" -f "$compose_file" ps
  fi
  exit 0
fi

# Make project paths deterministic and avoid Compose resolving them relative to
# whatever directory the operator happened to be in.
if [ "$dry_run" != "1" ]; then mkdir -p "$project_dir" "$(dirname -- "$env_file")"; fi
copy_env_template
copy_compose_template
copy_support_files
if [ "$dry_run" != "1" ]; then chmod 600 "$env_file" 2>/dev/null || true; fi
cpa_data_dir="$(normalize_path_var CPA_DATA_DIR "$project_dir/data/cpa")"
cpamp_data_dir="$(normalize_path_var CPAMP_DATA_DIR "$project_dir/data/manager")"
stack_root="$(normalize_path_var CPAMP_STACK_ROOT "$project_dir")"
backup_root="$(normalize_path_var CPAMP_BACKUP_ROOT "$project_dir/backups")"
admin_key_file="$(normalize_path_var CPAMP_ADMIN_KEY_FILE "$project_dir/secrets/cpamp-admin-key")"
management_key_file="$(normalize_path_var CPA_MANAGEMENT_KEY_FILE "$project_dir/secrets/cpa-management-key")"
license_secret_path="$(value_or CPA_LICENSE_CLIENT_SECRET_HOST_PATH '')"
if [ -z "$license_secret_path" ]; then license_secret_path="$(value_or CPA_LICENSE_CLIENT_SECRET_FILE "$project_dir/secrets/cpa-license-client-secret")"; fi
license_secret_path="$(resolve_path "$license_secret_path")"
set_env CPA_LICENSE_CLIENT_SECRET_HOST_PATH "$license_secret_path"

if [ "$dry_run" != "1" ]; then
  mkdir -p "$cpa_data_dir" "$cpa_data_dir/auths" "$cpa_data_dir/logs" "$cpa_data_dir/license" \
    "$cpamp_data_dir" "$stack_root" "$backup_root" "$(dirname -- "$admin_key_file")" \
    "$(dirname -- "$management_key_file")" "$(dirname -- "$license_secret_path")"
  chmod 700 "$(dirname -- "$admin_key_file")" "$(dirname -- "$management_key_file")" "$(dirname -- "$license_secret_path")" 2>/dev/null || true
fi

# Generate/reuse credentials. Existing secret files win over stale .env values,
# which keeps upgrades connected to the same CPA and CPAMP instances.
admin_key="$(ensure_file_secret CPA_MANAGER_ADMIN_KEY CPAMP_ADMIN_KEY_FILE "$project_dir/secrets/cpamp-admin-key" cpamp)"
management_key="$(ensure_file_secret CPA_MANAGEMENT_KEY CPA_MANAGEMENT_KEY_FILE "$project_dir/secrets/cpa-management-key" cpa)"
agent_token="$(ensure_env_secret CPAMP_AGENT_TOKEN cpamp_agent)"
if [ -z "$(value_or CPA_DEMO_API_KEY '')" ] || is_placeholder "$(value_or CPA_DEMO_API_KEY '')"; then
  set_env CPA_DEMO_API_KEY "sk-$(random_alnum 64)"
fi

# Migrate a legacy direct/file client secret into the canonical local secret.
# A storefront-backed deployment must receive the matching secret issued by
# the storefront.  In particular, never fill this file with a generated value:
# such a value looks configured to Compose but is rejected by /licenses/grace.
legacy_secret="$(value_or CPA_LICENSE_CLIENT_SECRET '')"
legacy_source="$(value_or CPA_LICENSE_CLIENT_SECRET_FILE '')"
if [ -n "$legacy_source" ] && [ "${legacy_source#/}" = "$legacy_source" ]; then legacy_source="$project_dir/${legacy_source#./}"; fi
license_secret_ready=0
license_secret_value=""
if [ -f "$license_secret_path" ]; then
  if [ ! -r "$license_secret_path" ]; then
    if storefront_secret_required; then
      die "Storefront client secret file is not readable: $license_secret_path. Inject the matching storefront-issued secret and rerun bootstrap (the value is never printed)."
    fi
  elif license_secret_value="$(trim_file_value "$license_secret_path" 2>/dev/null)" && ! is_blank_or_placeholder_secret "$license_secret_value"; then
    license_secret_ready=1
    # Existing files may have been copied with permissive permissions. Tighten
    # them before Compose mounts the Docker secret.
    if [ "$dry_run" != "1" ]; then chmod 600 "$license_secret_path"; fi
  fi
fi

if [ "$license_secret_ready" -ne 1 ] && [ -n "$legacy_source" ] && [ "$legacy_source" != "$license_secret_path" ]; then
  if [ -r "$legacy_source" ] && [ -f "$legacy_source" ] &&
     legacy_source_value="$(trim_file_value "$legacy_source" 2>/dev/null)" &&
     ! is_blank_or_placeholder_secret "$legacy_source_value"; then
    if [ "$dry_run" != "1" ]; then
      cp "$legacy_source" "$license_secret_path"
      chmod 600 "$license_secret_path"
    fi
    license_secret_ready=1
  elif [ "$dry_run" != "1" ] && storefront_secret_required && [ -e "$legacy_source" ]; then
    die "Storefront client secret source is not a readable, single-line secret: $legacy_source. Inject the matching storefront-issued secret and rerun bootstrap (the value is never printed)."
  fi
fi

if [ "$license_secret_ready" -ne 1 ] && ! is_blank_or_placeholder_secret "$legacy_secret"; then
  case "$legacy_secret" in
    *$'\n'*|*$'\r'*) die "CPA_LICENSE_CLIENT_SECRET must be a single-line secret" ;;
  esac
  if [ "$dry_run" != "1" ]; then
    printf '%s\n' "$legacy_secret" > "$license_secret_path"
    chmod 600 "$license_secret_path"
  fi
  license_secret_ready=1
fi

if [ "$license_secret_ready" -ne 1 ]; then
  if storefront_secret_required; then
    if [ "$dry_run" = "1" ]; then
      warn "Storefront client secret is required for this deployment; dry-run will not create a placeholder. Inject the matching secret at $license_secret_path before starting CPA."
    else
      die "Storefront client secret is required for CPA_LICENSE_PROVIDER=shop666 or p.666ttt.net. Inject the matching storefront-issued secret at $license_secret_path (one line, mode 600), then rerun bootstrap; the value is never printed."
    fi
  elif [ "$dry_run" != "1" ]; then
    # Local/non-storefront providers remain compatible with key-only installs.
    # Keep a real mode-600 Docker secret file, but never treat it as a
    # storefront credential.
    : > "$license_secret_path"
    chmod 600 "$license_secret_path"
  fi
fi
# Keep the legacy variable in .env empty after migration; compose still accepts
# it for users who run an older file without this bootstrap step.
set_env CPA_LICENSE_CLIENT_SECRET_FILE ""
set_env CPA_LICENSE_CLIENT_SECRET ""

# Defaults that must be present before preflight and Compose interpolation.
set_env CPA_IMAGE "$(value_or CPA_IMAGE ghcr.io/abc124774961/cli-proxy-api-cpa:v7.2.148-cpa.3)"
set_env CPAMP_IMAGE "$(value_or CPAMP_IMAGE ghcr.io/seakee/cpa-manager-plus:v1.12.8)"
set_env CPA_PULL_POLICY "$(value_or CPA_PULL_POLICY always)"
set_env CPAMP_PULL_POLICY "$(value_or CPAMP_PULL_POLICY always)"
set_env CPA_PORT "$(value_or CPA_PORT 8317)"
set_env CPAMP_PORT "$(value_or CPAMP_PORT 18317)"
set_env CPAMP_INTERNAL_PORT "$(value_or CPAMP_INTERNAL_PORT 18317)"
set_env CPAMP_AGENT_PORT "$(value_or CPAMP_AGENT_PORT 18417)"
set_env CPAMP_NETWORK_NAME "$(value_or CPAMP_NETWORK_NAME cpamp-cpa_default)"
set_env CPA_LICENSE_PUBLIC_KEY "$(value_or CPA_LICENSE_PUBLIC_KEY kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg)"
set_env CPA_LICENSE_PLUGIN_PUBLIC_KEY "$(value_or CPA_LICENSE_PLUGIN_PUBLIC_KEY OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ)"
set_env CPA_LICENSE_PROVIDER "$(value_or CPA_LICENSE_PROVIDER shop666)"
set_env CPA_LICENSE_PRODUCT_CODE "$(value_or CPA_LICENSE_PRODUCT_CODE CPA)"
set_env CPA_LICENSE_API_BASE_URL "$(value_or CPA_LICENSE_API_BASE_URL https://p.666ttt.net/api/storefront)"
set_env CPA_LICENSE_STATE_DIR "$(value_or CPA_LICENSE_STATE_DIR /CLIProxyAPI/data/license)"
set_env CPA_LICENSE_SHOP_AUTH_URL "$(value_or CPA_LICENSE_SHOP_AUTH_URL 'https://p.666ttt.net/shop/?authorize=cpa')"
set_env CPA_LICENSE_SHOP_EXCHANGE_PATH "$(value_or CPA_LICENSE_SHOP_EXCHANGE_PATH /licenses/exchange)"
set_env CPA_LICENSE_ACTIVATE_PATH "$(value_or CPA_LICENSE_ACTIVATE_PATH /licenses/activate)"
set_env CPA_LICENSE_REFRESH_PATH "$(value_or CPA_LICENSE_REFRESH_PATH /licenses/refresh)"
set_env CPA_LICENSE_VERIFY_PATH "$(value_or CPA_LICENSE_VERIFY_PATH /licenses/verify)"
set_env CPA_LICENSE_GRACE_PATH "$(value_or CPA_LICENSE_GRACE_PATH /licenses/grace)"
set_env CPA_LICENSE_REFRESH_INTERVAL "$(value_or CPA_LICENSE_REFRESH_INTERVAL 10m)"
set_env CPA_LICENSE_GRACE_PERIOD "$(value_or CPA_LICENSE_GRACE_PERIOD 6h)"
set_env CPA_LICENSE_CLIENT_ID "$(value_or CPA_LICENSE_CLIENT_ID '')"
set_env CPA_LICENSE_STORAGE_KEY "$(value_or CPA_LICENSE_STORAGE_KEY '')"
set_env CPA_LICENSE_EXECUTABLE_SHA256 "$(value_or CPA_LICENSE_EXECUTABLE_SHA256 '')"
set_env CPA_LICENSE_CLAIM_PATH "$(value_or CPA_LICENSE_CLAIM_PATH '')"

config_file="$cpa_data_dir/config.yaml"
render_config "$config_file"

preflight_compose_file="$compose_file"
if [ "$dry_run" = "1" ] && [ ! -f "$preflight_compose_file" ]; then
  preflight_compose_file="$script_dir/compose.yml"
fi
preflight_args=(--env-file "$env_file" --compose-file "$preflight_compose_file")
if [ "$dry_run" = "1" ] || [ "$action" = "render" ]; then preflight_args+=(--skip-docker); fi
if [ "$dry_run" = "1" ]; then
  info "Dry-run enabled: no files, images, containers, or data were changed"
else
  info "Running pool-server preflight"
fi
if [ "$dry_run" = "1" ]; then
  # The temporary .env has generated values but no secret files; use a file-only
  # preflight after creating temporary placeholders in the same directory.
  if [ -z "$tmp_env" ]; then tmp_env="$env_file"; fi
fi
if [ "$dry_run" != "1" ]; then
  "$script_dir/preflight.sh" "${preflight_args[@]}"
else
  # Dry-run validates the template and interpolation shape without requiring
  # the host Docker daemon or creating secret files.
  "$script_dir/preflight.sh" --env-file "$env_file" --compose-file "$preflight_compose_file" --skip-docker --allow-missing-secrets || true
fi

if [ "$action" = "render" ] || [ "$no_start" = "1" ]; then
  info "Pool stack files are ready; services were not started"
  exit 0
fi

if [ "$dry_run" = "1" ]; then
  if [ "$action" = "pull" ]; then
    info "Would run: docker compose --env-file '$env_file' -f '$compose_file' pull"
  else
    info "Would run: docker compose --env-file '$env_file' -f '$compose_file' up -d"
  fi
  exit 0
fi

command_exists docker || die "docker command is required to start the pool stack"
if [ "$action" = "pull" ]; then
  info "Pulling pinned CPA and CPAMP images"
  docker compose --env-file "$env_file" -f "$compose_file" pull
  info "Images pulled; services were not started"
  exit 0
fi
if [ "$no_pull" != "1" ]; then
  info "Pulling pinned CPA and CPAMP images"
  docker compose --env-file "$env_file" -f "$compose_file" pull
fi
info "Starting CPA, cpamp-agent, and CPAMP"
docker compose --env-file "$env_file" -f "$compose_file" up -d

cpa_name="$(value_or CPA_CONTAINER_NAME cli-proxy-api)"
cpamp_name="$(value_or CPAMP_CONTAINER_NAME cpa-manager-plus)"
agent_name="$(value_or CPAMP_AGENT_CONTAINER_NAME cpamp-agent)"
wait_for_health() {
  local name="$1"
  local timeout="$2"
  local elapsed=0
  local state=""
  while [ "$elapsed" -lt "$timeout" ]; do
    state="$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$name" 2>/dev/null || true)"
    case "$state" in
      "running healthy"*|"running ") return 0 ;;
    esac
    case "$state" in
      *unhealthy*|"exited"*|"dead"*)
        warn "$name reported state: $state"
        return 1
        ;;
    esac
    sleep 2
    elapsed=$((elapsed + 2))
  done
  warn "$name did not become healthy within ${timeout}s (last state: $state)"
  return 1
}
health_ok=1
wait_for_health "$cpa_name" "$health_timeout" || health_ok=0
wait_for_health "$agent_name" "$health_timeout" || health_ok=0
wait_for_health "$cpamp_name" "$health_timeout" || health_ok=0
docker compose --env-file "$env_file" -f "$compose_file" ps
if [ "$health_ok" -ne 1 ]; then
  die "Pool stack started but one or more health checks failed; inspect docker compose logs"
fi
printf 'Pool stack is healthy. Panel: http://<host>:%s/management.html\n' "$(value_or CPAMP_PORT 18317)"
printf 'Admin key file: %s\nCPA Management Key file: %s\n' \
  "$(value_or CPAMP_ADMIN_KEY_FILE "$project_dir/secrets/cpamp-admin-key")" \
  "$(value_or CPA_MANAGEMENT_KEY_FILE "$project_dir/secrets/cpa-management-key")"
