# Zed Provider Plugin

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that turns a **Zed account** into an OpenAI/Claude/Gemini-compatible API endpoint. It reproduces Zed's own desktop-client sign-in (browser OAuth with an RSA-encrypted loopback callback), mints per-organization LLM tokens, and proxies Zed's cloud completions with streaming, tool calls, and model discovery.

## How it maps to Zed

| Zed concept | Plugin behaviour |
|---|---|
| `GET /native_app_signin` browser sign-in | `StartLogin` opens a one-shot `127.0.0.1` listener, generates an RSA-2048 keypair, and returns the same URL the desktop app opens |
| RSA-encrypted `access_token` callback | The plugin decrypts the callback token (OAEP-SHA256, PKCS#1 v1.5 fallback) |
| `GET /client/users/me` → `organizations` | **One credential file per organization**, default organization first — Zed bills AI usage per organization, and a single account may belong to several |
| `POST /client/llm_tokens` `{organization_id}` | The executor mints a short-lived LLM token per credential and caches it in memory; it re-mints on `401` / `x-zed-expired-token` / `x-zed-outdated-token` |
| `GET /models` | Published on `/v1/models` per credential (`ExecutorModelScope: oauth`) |
| `POST /completions` (NDJSON stream wrapping native provider events) | Translated to native Claude / OpenAI Responses / OpenAI Chat / Gemini SSE for the caller |
| `POST /count_tokens` | Supported for Anthropic and OpenAI model families |

Zed's cloud always streams, so non-streaming downstream requests are aggregated per provider wire format before translation.

## Requirements

- CLIProxyAPI `v7.3.9`+ (plugin schema 6, ABI 1).
- Go 1.26+ and a C compiler for source builds.

## Build

macOS (arm64 shown):

```bash
go test ./...
go build -trimpath -buildmode=c-shared -ldflags "-X main.pluginVersion=0.1.0" -o dist/zed-v0.1.0.dylib ./cmd/zed
```

Note: on very new macOS SDKs (27.0) the Go linker may reject the SDK's `.tbd` files; pin the SDK with `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk` and matching `CGO_CFLAGS`/`CGO_LDFLAGS` `-isysroot` if you hit `tapi error: unknown architecture`.

Linux:

```bash
go build -trimpath -buildmode=c-shared -o dist/zed.so ./cmd/zed
```

## Install

Copy the library into CPA's `plugins` directory (or `plugins/darwin/arm64/`) and enable plugins in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    zed:
      enabled: true
      # Optional settings:
      # server-url: https://zed.dev        # sign-in server
      # cloud-url: https://cloud.zed.dev   # cloud API base
      # client-version: "1.23.0"           # User-Agent / x-zed-version
      # organization-id: <org-id>          # only save this organization at login
      # oauth-callback-port: 18520         # pin the loopback callback port
```

The plugin id comes from the library file name (`zed[-v<version>].dylib`) and must match the `plugins.configs.zed` key.

## Login (browser click-through, remote-host friendly)

Management Center: click OAuth login for **zed** — the panel opens the plugin's
own relay page (`/v0/resource/plugins/zed/login?state=...`), which opens the
real Zed sign-in and then captures the callback. Because Zed always redirects
the callback to `http://127.0.0.1:...` on the *browser's* machine, a remote CPA
host (e.g. Render) can never see it: the relay page asks you to paste that
address-bar URL back, completes the sign-in server-side, and saves one
credential file per organization. The Management Center poll then reports
success. When the browser happens to run on the CPA host itself, the loopback
listener captures the callback automatically and the paste step is unnecessary.

CLI equivalent:

```bash
./cli-proxy-api --config config.yaml --zed-login
```

Credentials carry no refresh token — Zed's desktop client re-signs in through
the browser when its long-lived token is rejected. The plugin re-validates each
credential every 6 hours via `GET /client/users/me`; when that starts returning
401, run the login again.

## Configuration reference

| Key | Default | Meaning |
|---|---|---|
| `server-url` | `https://zed.dev` | Sign-in server base URL (env `ZED_SERVER_URL`) |
| `cloud-url` | `https://cloud.zed.dev` | Cloud API base URL (env `ZED_CLOUD_URL`) |
| `client-version` | `1.23.0` | App version in `User-Agent: Zed/{v} ({os}; {arch})` and `x-zed-version` (env `ZED_CLIENT_VERSION`) |
| `organization-id` | unset | Restrict login to one organization (env `ZED_ORGANIZATION_ID`) |
| `oauth-callback-port` | ephemeral | Pin the `127.0.0.1` callback port (env `ZED_OAUTH_CALLBACK_PORT`) |

## Security notes

- `auth-dir` contains long-lived Zed access tokens; protect it.
- The plugin registers no HTTP routes of its own. Its only listener is the one-shot sign-in callback on the CPA host's `127.0.0.1`.

Licensed under the MIT License.
