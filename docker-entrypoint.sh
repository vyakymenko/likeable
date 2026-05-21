#!/usr/bin/env sh

set -eu

database_path="${DATABASE_PATH:-/data/likeable.db}"
if [ -n "${LIKEABLE_DATA_DIR:-}" ]; then
  data_dir="${LIKEABLE_DATA_DIR}"
else
  case "$database_path" in
    */*) data_dir="${database_path%/*}" ;;
    *) data_dir="." ;;
  esac
fi

RUNTIME_FIBE_BIN_DIR="${LIKEABLE_FIBE_BIN_DIR:-${data_dir}/.fibe/bin}"
RUNTIME_FIBE_BIN="${RUNTIME_FIBE_BIN_DIR}/fibe"
RUNTIME_FIBE_WRAPPER="${RUNTIME_FIBE_BIN_DIR}/fibe-likeable-wrapper"
ACTIVE_FIBE_WRAPPER=""
export PATH="${RUNTIME_FIBE_BIN_DIR}:$PATH"

fibe_binary_version() {
  binary="$1"
  if [ -x "$binary" ]; then
    "$binary" version 2>/dev/null | awk 'NR == 1 { print $2 }'
  fi
}

latest_fibe_version() {
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL --retry 2 --connect-timeout 10 --max-time 60 \
      -H "Authorization: Bearer ${GH_TOKEN}" \
      -H "Accept: application/vnd.github+json" \
      https://api.github.com/repos/fibegg/sdk/releases/latest \
      | jq -r '.tag_name // empty' \
      | sed 's/^v//'
  else
    curl -fsSL --retry 2 --connect-timeout 10 --max-time 60 \
      -H "Accept: application/vnd.github+json" \
      https://api.github.com/repos/fibegg/sdk/releases/latest \
      | jq -r '.tag_name // empty' \
      | sed 's/^v//'
  fi
}

install_runtime_fibe() {
  desired_version="$1"
  installer=""
  for candidate in /usr/local/bin/install-fibe.sh /app/scripts/install-fibe.sh; do
    if [ -f "$candidate" ]; then
      installer="$candidate"
      break
    fi
  done

  if [ -z "$installer" ]; then
    return 1
  fi

  if [ -n "$desired_version" ]; then
    FIBE_INSTALL_DIR="$RUNTIME_FIBE_BIN_DIR" FIBE_VERSION="$desired_version" sh "$installer"
  else
    FIBE_INSTALL_DIR="$RUNTIME_FIBE_BIN_DIR" sh "$installer"
  fi
}

copy_baked_fibe() {
  if [ ! -x /usr/local/bin/fibe ]; then
    return 1
  fi
  echo "[entrypoint] Copying baked fibe from /usr/local/bin/fibe"
  mkdir -p "$RUNTIME_FIBE_BIN_DIR"
  cp /usr/local/bin/fibe "$RUNTIME_FIBE_BIN"
  chmod +x "$RUNTIME_FIBE_BIN"
}

ensure_runtime_fibe() {
  mkdir -p "$RUNTIME_FIBE_BIN_DIR"

  current_version="$(fibe_binary_version "$RUNTIME_FIBE_BIN" || true)"
  requested_version="${FIBE_VERSION:-${FIBE_CLI_VERSION:-}}"
  desired_version="${requested_version#v}"

  if [ -z "$desired_version" ]; then
    desired_version="$(latest_fibe_version 2>/dev/null || true)"
    if [ -z "$desired_version" ] && [ -n "$current_version" ]; then
      echo "[entrypoint] Could not resolve latest fibe release; using cached runtime fibe ${current_version}"
      export LIKEABLE_FIBE_VERSION="$current_version"
      return
    fi
  fi

  if [ -n "$desired_version" ] && [ "$current_version" = "$desired_version" ]; then
    echo "[entrypoint] Using cached runtime fibe ${current_version}"
    export LIKEABLE_FIBE_VERSION="$current_version"
    return
  fi

  if [ -n "$desired_version" ]; then
    echo "[entrypoint] Installing runtime fibe ${desired_version}"
  else
    echo "[entrypoint] Installing runtime fibe latest"
  fi

  if ! install_runtime_fibe "$desired_version"; then
    if [ -n "$current_version" ]; then
      echo "[entrypoint] Runtime fibe install failed; using cached runtime fibe ${current_version}" >&2
    elif copy_baked_fibe; then
      true
    else
      echo "[entrypoint] ERROR: no runtime, installer, or baked fibe binary is available" >&2
      exit 1
    fi
  fi

  installed_version="$(fibe_binary_version "$RUNTIME_FIBE_BIN" || true)"
  if [ -z "$installed_version" ]; then
    echo "[entrypoint] ERROR: runtime fibe is not executable" >&2
    exit 1
  fi
  export LIKEABLE_FIBE_VERSION="$installed_version"
  echo "[entrypoint] Runtime fibe ready: ${installed_version}"
}

sync_runtime_fibe_wrapper() {
  if [ ! -x /usr/local/bin/fibe-likeable-wrapper ]; then
    return
  fi
  mkdir -p "$RUNTIME_FIBE_BIN_DIR"
  if cp /usr/local/bin/fibe-likeable-wrapper "$RUNTIME_FIBE_WRAPPER" 2>/dev/null; then
    chmod +x "$RUNTIME_FIBE_WRAPPER"
    ACTIVE_FIBE_WRAPPER="$RUNTIME_FIBE_WRAPPER"
  elif [ -x "$RUNTIME_FIBE_WRAPPER" ] && cmp -s /usr/local/bin/fibe-likeable-wrapper "$RUNTIME_FIBE_WRAPPER"; then
    ACTIVE_FIBE_WRAPPER="$RUNTIME_FIBE_WRAPPER"
  else
    echo "[entrypoint] Could not update runtime fibe wrapper; using baked wrapper" >&2
    ACTIVE_FIBE_WRAPPER="/usr/local/bin/fibe-likeable-wrapper"
  fi
}

ensure_runtime_fibe
sync_runtime_fibe_wrapper

if [ -z "${FIBE_CLI_PATH:-}" ] && [ -n "$ACTIVE_FIBE_WRAPPER" ] && [ -x "$ACTIVE_FIBE_WRAPPER" ]; then
  export FIBE_REAL_CLI="$RUNTIME_FIBE_BIN"
  export FIBE_CLI_PATH="$ACTIVE_FIBE_WRAPPER"
else
  export FIBE_CLI_PATH="${FIBE_CLI_PATH:-$RUNTIME_FIBE_BIN}"
fi

if [ "$#" -eq 0 ]; then
  set -- likeable
fi

exec "$@"
