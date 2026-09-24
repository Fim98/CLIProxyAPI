#!/bin/sh
# Runtime entrypoint for Render.
# - Rewrites `port:` from $PORT (Render injects PORT, default 10000).
# - Injects $MANAGEMENT_KEY / $API_KEY into config placeholders so secrets
#   never live in git (render.config.yaml carries __MANAGEMENT_KEY__ /
#   __API_KEY__ placeholders).
#
# Portable across GNU sed (Linux/Render) and BSD sed (macOS local test):
# all in-place edits go through a temp file, no `sed -i`.
set -eu

CONFIG="${CPA_CONFIG:-/CLIProxyAPI/config.yaml}"

if [ ! -f "$CONFIG" ]; then
  echo "entrypoint: config not found: $CONFIG" >&2
  exit 1
fi

replace_literal() {
  # $1 = literal pattern, $2 = literal replacement, $3 = file.
  # Uses awk index() for literal matching: no regex escaping issues
  # even if keys contain & | / or backslashes.
  pattern="$1"
  replacement="$2"
  file="$3"
  tmp="$file.tmp.$$"
  awk -v pat="$pattern" -v rep="$replacement" '
    {
      out = ""
      rest = $0
      while ((i = index(rest, pat)) > 0) {
        out = out substr(rest, 1, i - 1) rep
        rest = substr(rest, i + length(pat))
      }
      print out rest
    }
  ' "$file" > "$tmp"
  mv "$tmp" "$file"
}

# 1) Port: Render sets $PORT (e.g. 10000). CPA listens on `port:`.
# Only touch the top-level `port:` key: match lines starting with `port:`.
# (Indented keys like `oauth-callback-port:` start with spaces, untouched.)
if [ -n "${PORT:-}" ]; then
  tmp="$CONFIG.tmp.$$"
  awk -v p="$PORT" '{ if ($0 ~ /^port: /) { print "port: " p; next } print }' \
    "$CONFIG" > "$tmp"
  mv "$tmp" "$CONFIG"
  echo "entrypoint: port set to ${PORT}"
fi

# 2) Management key (required for /v0/management/* incl. plugin status).
if [ -n "${MANAGEMENT_KEY:-}" ]; then
  replace_literal "__MANAGEMENT_KEY__" "$MANAGEMENT_KEY" "$CONFIG"
  echo "entrypoint: management key injected"
else
  echo "entrypoint: WARNING: \$MANAGEMENT_KEY is empty; management API stays disabled until secret-key is set" >&2
fi

# 3) Proxy API key (clients call CPA with this).
if [ -n "${API_KEY:-}" ]; then
  replace_literal "__API_KEY__" "$API_KEY" "$CONFIG"
  echo "entrypoint: api key injected"
else
  echo "entrypoint: WARNING: \$API_KEY is empty; replace api-keys before use" >&2
fi

# 4) Fail fast if placeholders survived (means env vars were missing).
if grep -q "__MANAGEMENT_KEY__\|__API_KEY__" "$CONFIG"; then
  echo "entrypoint: ERROR: placeholders remain in $CONFIG; set MANAGEMENT_KEY and API_KEY env vars" >&2
  exit 1
fi

echo "entrypoint: plugins bundled:"
ls -lh /CLIProxyAPI/plugins/ || true

exec "$@"
