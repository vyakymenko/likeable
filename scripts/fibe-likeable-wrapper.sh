#!/usr/bin/env bash
set -euo pipefail

REAL_FIBE="${FIBE_REAL_CLI:-/data/.fibe/bin/fibe}"
if [[ ! -x "$REAL_FIBE" ]]; then
  REAL_FIBE="/usr/local/bin/fibe"
fi

fail_json() {
  local message="$1"
  local code="${2:-INTERNAL_ERROR}"
  local status="${3:-422}"
  case "$status" in
    ''|*[!0-9]*) status="422" ;;
  esac
  jq -n --arg message "$message" --arg code "$code" --argjson status "$status" \
    '{error:{code:$code,status:$status,message:$message}}' >&2
  exit 1
}

log_wrapper() {
  if [[ -d /data ]]; then
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>/data/fibe-likeable-wrapper.log 2>/dev/null || true
  fi
}

platform_code_for_status() {
  local status="$1"
  case "$status" in
    401) printf 'UNAUTHORIZED' ;;
    403) printf 'FORBIDDEN' ;;
    404) printf 'NOT_FOUND' ;;
    408) printf 'TIMEOUT' ;;
    409) printf 'CONFLICT' ;;
    422) printf 'VALIDATION_FAILED' ;;
    429) printf 'RATE_LIMITED' ;;
    5*) printf 'SERVICE_UNAVAILABLE' ;;
    *) printf 'INTERNAL_ERROR' ;;
  esac
}

compact_response_body() {
  local file="$1"
  tr '\n\r\t' '   ' <"$file" | sed 's/  */ /g' | cut -c 1-700
}

duration_seconds() {
  local value="$1"
  case "$value" in
    *h) printf '%s' "$(( ${value%h} * 3600 ))" ;;
    *m) printf '%s' "$(( ${value%m} * 60 ))" ;;
    *s) printf '%s' "${value%s}" ;;
    ''|*[!0-9]*) printf '600' ;;
    *) printf '%s' "$value" ;;
  esac
}

curl_http_status() {
  local response_file="$1"
  local error_file status
  shift
  error_file="$(mktemp)"
  if status="$(curl -sS -o "$response_file" -w '%{http_code}' "$@" 2>"$error_file")"; then
    rm -f "$error_file"
    printf '%s' "$status"
    return 0
  fi
  compact_response_body "$error_file"
  rm -f "$error_file"
  return 1
}

base_url_from_domain() {
  local value="$1"
  if [[ "$value" != http://* && "$value" != https://* ]]; then
    value="https://$value"
  fi
  printf '%s' "${value%/}"
}

normalize_codex_auth_token() {
  local token="$1"
  jq -nrc --arg token "$token" '
    def normalize:
      if type == "object" then
        . as $root
        | (if (($root.access_token? // $root.token? // $root.api_key? // "") == "" and ($root.tokens.access_token? // "") != "") then
            . + {access_token: $root.tokens.access_token}
          else
            .
          end)
        | . as $root
        | if (($root.api_key? // "") == "" and ($root.OPENAI_API_KEY? // "") != "") then
            . + {api_key: $root.OPENAI_API_KEY}
          else
            .
          end
      else
        .
      end;
    try ($token | fromjson | normalize | tojson) catch $token
  '
}

api_json() {
  local method="$1"
  local url="$2"
  local payload="${3:-}"
  local response_file status message
  response_file="$(mktemp)"
  if [[ -n "$payload" ]]; then
    if ! status="$(curl_http_status "$response_file" \
      -X "$method" \
      -H "Authorization: Bearer $api_key" \
      -H "Accept: application/json" \
      -H "Content-Type: application/json" \
      --data-binary "$payload" \
      "$url")"; then
      message="$status"
      rm -f "$response_file"
      [[ -n "$message" ]] || message="Fibe API request failed before receiving a response"
      fail_json "$message" "SERVICE_UNAVAILABLE" "503"
    fi
  else
    if ! status="$(curl_http_status "$response_file" \
      -X "$method" \
      -H "Authorization: Bearer $api_key" \
      -H "Accept: application/json" \
      "$url")"; then
      message="$status"
      rm -f "$response_file"
      [[ -n "$message" ]] || message="Fibe API request failed before receiving a response"
      fail_json "$message" "SERVICE_UNAVAILABLE" "503"
    fi
  fi
  if [[ "$status" != 2* ]]; then
    message="$(jq -r '.error.message? // .message? // empty' "$response_file" 2>/dev/null || true)"
    [[ -n "$message" ]] || message="Fibe API request failed ($status)"
    rm -f "$response_file"
    fail_json "$message" "$(platform_code_for_status "$status")" "$status"
  fi
  cat "$response_file"
  rm -f "$response_file"
}

handle_agents() {
  local subcommand="${1:-}"
  [[ -n "$subcommand" ]] || exec "$REAL_FIBE" "${original[@]}"
  shift || true

  case "$subcommand" in
    start-chat)
      local agent_id="${1:-}" marquee_id="" force="false" payload
      [[ -n "$agent_id" ]] || fail_json "agent id is required"
      shift || true
      while (($#)); do
        case "$1" in
          --marquee-id)
            marquee_id="${2:-}"
            shift 2
            ;;
          --force)
            force="true"
            shift
            ;;
          *)
            shift
            ;;
        esac
      done
      [[ -n "$domain" ]] || fail_json "Fibe domain is not configured"
      [[ -n "$api_key" ]] || fail_json "Fibe API key is not configured"
      [[ -n "$marquee_id" ]] || fail_json "marquee id is required"
      payload="$(jq -n --arg marquee_id "$marquee_id" --argjson force "$force" \
        '{marquee_id:($marquee_id|tonumber? // $marquee_id), force:$force}')"
      log_wrapper "agents start-chat agent_id=$agent_id marquee_id=$marquee_id force=$force"
      api_json POST "$(base_url_from_domain "$domain")/api/agents/$agent_id/chats" "$payload"
      exit 0
      ;;
    authenticate)
      local agent_id="${1:-}" code="" token="" payload
      [[ -n "$agent_id" ]] || fail_json "agent id is required"
      shift || true
      while (($#)); do
        case "$1" in
          --code)
            code="${2:-}"
            shift 2
            ;;
          --token)
            token="${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      [[ -n "$domain" ]] || fail_json "Fibe domain is not configured"
      [[ -n "$api_key" ]] || fail_json "Fibe API key is not configured"
      [[ -n "$code$token" ]] || fail_json "agent auth code or token is required"
      if [[ -n "$token" ]]; then
        token="$(normalize_codex_auth_token "$token")"
      fi
      payload="$(jq -n --arg code "$code" --arg token "$token" \
        'if $code != "" then {code:$code} else {token:$token} end')"
      log_wrapper "agents authenticate agent_id=$agent_id"
      api_json PUT "$(base_url_from_domain "$domain")/api/agents/$agent_id/auth" "$payload"
      exit 0
      ;;
    send-message|chat)
      local agent_id="${1:-}" text="" conversation_id="" busy_policy="" from_file="" file_payload="{}" payload
      local attachment_filenames="[]" upload_file upload_status upload_response upload_name
      local -a attach_paths=("__FIBE_WRAPPER_NO_ATTACH__")
      [[ -n "$agent_id" ]] || fail_json "agent id is required"
      shift || true
      while (($#)); do
        case "$1" in
          --text)
            text="${2:-}"
            shift 2
            ;;
          --conversation-id)
            conversation_id="${2:-}"
            shift 2
            ;;
          --busy-policy)
            busy_policy="${2:-}"
            shift 2
            ;;
          --attachment-filename)
            attachment_filenames="$(jq -c --arg filename "${2:-}" '. + [$filename]' <<<"$attachment_filenames")"
            shift 2
            ;;
          --attach)
            attach_paths+=("${2:-}")
            shift 2
            ;;
          -f|--from-file)
            from_file="${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      if [[ -n "$from_file" ]]; then
        if [[ "$from_file" == "-" ]]; then
          file_payload="$(cat)"
        else
          file_payload="$(cat "$from_file")"
        fi
      fi
      [[ -n "$text" ]] || text="$(jq -r '.text // empty' <<<"$file_payload")"
      [[ -n "$conversation_id" ]] || conversation_id="$(jq -r '.conversation_id // .conversationId // empty' <<<"$file_payload")"
      [[ -n "$busy_policy" ]] || busy_policy="$(jq -r '.busy_policy // .busyPolicy // empty' <<<"$file_payload")"
      attachment_filenames="$(jq -c --argjson current "$attachment_filenames" '
        $current + [(.attachmentFilenames // [])[], (.attachment_filenames // [])[]]
      ' <<<"$file_payload")"
      [[ -n "$domain" ]] || fail_json "Fibe domain is not configured"
      [[ -n "$api_key" ]] || fail_json "Fibe API key is not configured"
      [[ -n "$text" ]] || fail_json "message text is required"
      for upload_file in "${attach_paths[@]}"; do
        [[ "$upload_file" != "__FIBE_WRAPPER_NO_ATTACH__" ]] || continue
        [[ -n "$upload_file" ]] || continue
        [[ -f "$upload_file" ]] || fail_json "attachment file was not found: $upload_file" "VALIDATION_FAILED" "422"
        upload_response="$(mktemp)"
        if [[ -n "$conversation_id" ]]; then
          if ! upload_status="$(curl_http_status "$upload_response" \
            -X POST \
            -H "Authorization: Bearer $api_key" \
            -H "Accept: application/json" \
            -F "conversation_id=$conversation_id" \
            -F "file=@$upload_file" \
            "$(base_url_from_domain "$domain")/api/agents/$agent_id/uploads")"; then
            message="$upload_status"
            rm -f "$upload_response"
            [[ -n "$message" ]] || message="attachment upload failed before receiving a response"
            fail_json "$message" "SERVICE_UNAVAILABLE" "503"
          fi
        else
          if ! upload_status="$(curl_http_status "$upload_response" \
            -X POST \
            -H "Authorization: Bearer $api_key" \
            -H "Accept: application/json" \
            -F "file=@$upload_file" \
            "$(base_url_from_domain "$domain")/api/agents/$agent_id/uploads")"; then
            message="$upload_status"
            rm -f "$upload_response"
            [[ -n "$message" ]] || message="attachment upload failed before receiving a response"
            fail_json "$message" "SERVICE_UNAVAILABLE" "503"
          fi
        fi
        if [[ "$upload_status" != 2* ]]; then
          local message
          message="$(jq -r '.error.message? // .message? // empty' "$upload_response" 2>/dev/null || true)"
          rm -f "$upload_response"
          [[ -n "$message" ]] || message="attachment upload failed ($upload_status)"
          fail_json "$message" "$(platform_code_for_status "$upload_status")" "$upload_status"
        fi
        upload_name="$(jq -r '.filename // empty' "$upload_response")"
        rm -f "$upload_response"
        [[ -n "$upload_name" ]] || fail_json "attachment upload did not return a filename"
        attachment_filenames="$(jq -c --arg filename "$upload_name" '. + [$filename]' <<<"$attachment_filenames")"
      done
      payload="$(jq -n \
        --arg text "$text" \
        --arg conversation_id "$conversation_id" \
        --arg busy_policy "$busy_policy" \
        --argjson attachment_filenames "$attachment_filenames" \
        '{text:$text}
          + (if $conversation_id != "" then {conversation_id:$conversation_id} else {} end)
          + (if $busy_policy != "" then {busy_policy:$busy_policy} else {} end)
          + (if ($attachment_filenames | length) > 0 then {attachmentFilenames:$attachment_filenames} else {} end)')"
      log_wrapper "agents send-message agent_id=$agent_id conversation_id=$conversation_id attachments=$(jq -r 'length' <<<"$attachment_filenames")"
      api_json POST "$(base_url_from_domain "$domain")/api/agents/$agent_id/messages" "$payload"
      exit 0
      ;;
    *)
      exec "$REAL_FIBE" "${original[@]}"
      ;;
  esac
}

original=("$@")
domain="${FIBE_DOMAIN:-}"
api_key="${FIBE_API_KEY:-}"
output="json"
cmd=""

while (($#)); do
  case "$1" in
    --domain)
      domain="${2:-}"
      shift 2
      ;;
    --api-key)
      api_key="${2:-}"
      shift 2
      ;;
    --output|-o)
      output="${2:-json}"
      shift 2
      ;;
    --debug|--explain-errors)
      shift
      ;;
    --*)
      if (($# >= 2)) && [[ "${2:-}" != --* ]]; then
        shift 2
      else
        shift
      fi
      ;;
    *)
      cmd="$1"
      shift
      break
      ;;
  esac
done

if [[ "$cmd" == "agents" ]]; then
  handle_agents "$@"
fi

if [[ "$cmd" != "greenfield" ]]; then
  exec "$REAL_FIBE" "${original[@]}"
fi

name=""
marquee_id=""
template_version_id=""
wait_timeout="10m"
variables="{}"
service_subdomains="{}"

while (($#)); do
  case "$1" in
    --name)
      name="${2:-}"
      shift 2
      ;;
    --marquee-id)
      marquee_id="${2:-}"
      shift 2
      ;;
    --template-version-id)
      template_version_id="${2:-}"
      shift 2
      ;;
    --wait-timeout)
      wait_timeout="${2:-10m}"
      shift 2
      ;;
    --var)
      pair="${2:-}"
      key="${pair%%=*}"
      value="${pair#*=}"
      variables="$(jq -c --arg key "$key" --arg value "$value" '. + {($key): $value}' <<<"$variables")"
      shift 2
      ;;
    --service-subdomain|--git-provider)
      if [[ "$1" == "--service-subdomain" ]]; then
        pair="${2:-}"
        key="${pair%%=*}"
        value="${pair#*=}"
        service_subdomains="$(jq -c --arg key "$key" --arg value "$value" '. + {($key): $value}' <<<"$service_subdomains")"
      fi
      shift 2
      ;;
    --private)
      shift
      ;;
    *)
      shift
      ;;
  esac
done

[[ -n "$domain" ]] || fail_json "Fibe domain is not configured"
[[ -n "$api_key" ]] || fail_json "Fibe API key is not configured"
[[ -n "$name" ]] || fail_json "greenfield name is required"
[[ -n "$marquee_id" ]] || fail_json "greenfield marquee id is required"
log_wrapper "greenfield name=$name marquee_id=$marquee_id template_version_id=$template_version_id"

base_url="$(base_url_from_domain "$domain")"

if [[ -z "$template_version_id" ]]; then
  create_file="$(mktemp)"
  poll_file="$(mktemp)"
  trap 'rm -f "$create_file" "$poll_file"' EXIT
  template_body_path="${LIKEABLE_GREENFIELD_TEMPLATE_BODY_PATH:-/usr/local/share/likeable/go-fibe-greenfield.yaml}"
  if [[ -f "$template_body_path" ]]; then
    payload="$(jq -n \
      --arg name "$name" \
      --arg marquee_id "$marquee_id" \
      --argjson variables "$variables" \
      --argjson service_subdomains "$service_subdomains" \
      --rawfile template_body "$template_body_path" \
      '{name:$name, git_provider:"gitea", private:true, marquee_id:($marquee_id|tonumber? // $marquee_id), variables:$variables, service_subdomains:$service_subdomains, template_body:$template_body}')"
    log_wrapper "greenfield default API name=$name marquee_id=$marquee_id template_body=$template_body_path"
  else
    payload="$(jq -n \
      --arg name "$name" \
      --arg marquee_id "$marquee_id" \
      --argjson variables "$variables" \
      --argjson service_subdomains "$service_subdomains" \
      '{name:$name, git_provider:"gitea", private:true, marquee_id:($marquee_id|tonumber? // $marquee_id), variables:$variables, service_subdomains:$service_subdomains}')"
    log_wrapper "greenfield default API name=$name marquee_id=$marquee_id template_body=platform-default"
  fi
  if ! create_status="$(curl_http_status "$create_file" \
    -X POST \
    -H "Authorization: Bearer $api_key" \
    -H "Accept: application/json" \
    -H "Content-Type: application/json" \
    --data-binary "$payload" \
    "$base_url/api/greenfields")"; then
    message="$create_status"
    log_wrapper "greenfield create failed before response message=$message"
    [[ -n "$message" ]] || message="greenfield create failed before receiving a response"
    fail_json "$message" "SERVICE_UNAVAILABLE" "503"
  fi
  if [[ "$create_status" != 2* ]]; then
    message="$(jq -r '.error.message? // .message? // empty' "$create_file" 2>/dev/null || true)"
    error_code="$(jq -r '.error.code? // .code? // empty' "$create_file" 2>/dev/null || true)"
    error_status="$(jq -r '.error.status? // .status? // empty' "$create_file" 2>/dev/null || true)"
    [[ -n "$message" ]] || message="greenfield create failed ($create_status)"
    [[ -n "$error_status" ]] || error_status="$create_status"
    [[ -n "$error_code" ]] || error_code="$(platform_code_for_status "$error_status")"
    log_wrapper "greenfield create failed status=$create_status code=$error_code message=$message body=$(compact_response_body "$create_file")"
    fail_json "$message" "$error_code" "$error_status"
  fi
  if [[ "$create_status" == "202" ]]; then
    status_url="$(jq -r '.status_url // empty' "$create_file")"
    request_id="$(jq -r '.request_id // empty' "$create_file")"
    if [[ -z "$status_url" ]]; then
      [[ -n "$request_id" ]] || fail_json "greenfield create did not return an async request id"
      status_url="/api/async_requests/$request_id"
    fi
    if [[ "$status_url" == http://* || "$status_url" == https://* ]]; then
      poll_url="$status_url"
    else
      poll_url="$base_url/${status_url#/}"
    fi
    deadline=$(( $(date +%s) + $(duration_seconds "$wait_timeout") ))
    while true; do
      if ! poll_status="$(curl_http_status "$poll_file" \
        -H "Authorization: Bearer $api_key" \
        -H "Accept: application/json" \
        "$poll_url")"; then
        message="$poll_status"
        [[ -n "$message" ]] || message="greenfield async poll failed before receiving a response"
        fail_json "$message" "SERVICE_UNAVAILABLE" "503"
      fi
      if [[ "$poll_status" != 2* && "$poll_status" != "422" ]]; then
        message="$(jq -r '.error.message? // .message? // empty' "$poll_file" 2>/dev/null || true)"
        [[ -n "$message" ]] || message="greenfield async poll failed ($poll_status)"
        fail_json "$message" "$(platform_code_for_status "$poll_status")" "$poll_status"
      fi
      async_status="$(jq -r '.status // empty' "$poll_file" 2>/dev/null || true)"
      if [[ "$async_status" == "success" || ( -z "$async_status" && "$poll_status" == 2* ) ]]; then
        cp "$poll_file" "$create_file"
        break
      fi
      if [[ "$async_status" == "error" || "$poll_status" == "422" ]]; then
        message="$(jq -r '.error.message? // .message? // .error? // empty' "$poll_file" 2>/dev/null || true)"
        [[ -n "$message" ]] || message="greenfield async request failed"
        fail_json "$message" "REMOTE_REQUEST_FAILED" "422"
      fi
      if (( $(date +%s) >= deadline )); then
        fail_json "greenfield async request timed out after $wait_timeout" "TIMEOUT" "408"
      fi
      sleep 2
    done
  fi
  playground_id="$(jq -r '
    def first_object:
      if type == "array" then (.[0] // {})
      elif type == "object" then .
      else {}
      end;
    (.payload // .result // .) as $root
    | ($root.playground | first_object | .id) // $root.playground_id // $root.id // empty
  ' "$create_file")"
  [[ -n "$playground_id" ]] || fail_json "greenfield create did not return a playground id"
  log_wrapper "greenfield default playground_id=$playground_id wait_timeout=$wait_timeout"
  wait_file="$(mktemp)"
  if ! "$REAL_FIBE" --domain "$domain" --api-key "$api_key" --output "$output" \
    wait playground "$playground_id" --status running --timeout "$wait_timeout" --interval 8s >"$wait_file" 2>&1; then
    message="$(compact_response_body "$wait_file")"
    rm -f "$wait_file"
    [[ -n "$message" ]] || message="playground $playground_id did not reach running"
    log_wrapper "greenfield default playground_id=$playground_id wait failed message=$message"
    fail_json "$message" "TIMEOUT" "408"
  fi
  rm -f "$wait_file"
  log_wrapper "greenfield default playground_id=$playground_id running"
  jq '(.payload // .result // .)' "$create_file"
  exit 0
fi

templates_file="$(mktemp)"
launch_file="$(mktemp)"
trap 'rm -f "$templates_file" "$launch_file"' EXIT

if ! templates_status="$(curl_http_status "$templates_file" \
  -H "Authorization: Bearer $api_key" \
  -H "Accept: application/json" \
  "$base_url/api/import_templates?per_page=100")"; then
  message="$templates_status"
  log_wrapper "template lookup failed before response message=$message"
  [[ -n "$message" ]] || message="template lookup failed before receiving a response"
  fail_json "$message" "SERVICE_UNAVAILABLE" "503"
fi
if [[ "$templates_status" != 2* ]]; then
  message="$(jq -r '.error.message? // .message? // empty' "$templates_file" 2>/dev/null || true)"
  [[ -n "$message" ]] || message="template lookup failed ($templates_status)"
  log_wrapper "template lookup failed status=$templates_status body=$(compact_response_body "$templates_file")"
  fail_json "$message" "$(platform_code_for_status "$templates_status")" "$templates_status"
fi

template_id="$(jq -r --arg version_id "$template_version_id" '
  (if type == "array" then . else (.data // .Data // .items // []) end)
  | map(select(
      ((.latest_version_id // .latestVersionId // "") | tostring) == $version_id
      or (((.versions // [])
        | map(if type == "object" then ((.id // "") | tostring) else "" end)
        | index($version_id)) != null)
  ))
  | .[0].id // empty
' "$templates_file")"
[[ -n "$template_id" ]] || fail_json "template for version $template_version_id was not found" "TEMPLATE_VERSION_NOT_FOUND" "422"
template_system="$(jq -r --arg version_id "$template_version_id" '
  (if type == "array" then . else (.data // .Data // .items // []) end)
  | map(select(
      ((.latest_version_id // .latestVersionId // "") | tostring) == $version_id
      or (((.versions // [])
        | map(if type == "object" then ((.id // "") | tostring) else "" end)
        | index($version_id)) != null)
  ))
  | .[0].system // empty
' "$templates_file")"
template_name="$(jq -r --arg version_id "$template_version_id" '
  (if type == "array" then . else (.data // .Data // .items // []) end)
  | map(select(
      ((.latest_version_id // .latestVersionId // "") | tostring) == $version_id
      or (((.versions // [])
        | map(if type == "object" then ((.id // "") | tostring) else "" end)
        | index($version_id)) != null)
  ))
  | .[0].name // empty
' "$templates_file")"
if [[ "${LIKEABLE_ALLOW_NON_SYSTEM_TEMPLATE:-}" != "1" && "$template_system" == "false" ]]; then
  fail_json "configured template version $template_version_id belongs to non-system import template ${template_name:-$template_id}; clear fibe_template_version_id to use the Fibe greenfield default" "INVALID_TEMPLATE_VERSION" "422"
fi
log_wrapper "template_id=$template_id"

payload="$(jq -n \
  --arg name "$name" \
  --arg marquee_id "$marquee_id" \
  --argjson variables "$variables" \
  '{name:$name, marquee_id:($marquee_id|tonumber? // $marquee_id), variables:$variables}')"

if ! launch_status="$(curl_http_status "$launch_file" \
  -X POST \
  -H "Authorization: Bearer $api_key" \
  -H "Accept: application/json" \
  -H "Content-Type: application/json" \
  --data-binary "$payload" \
  "$base_url/api/import_templates/$template_id/launches")"; then
  message="$launch_status"
  log_wrapper "launch failed before response message=$message"
  [[ -n "$message" ]] || message="template launch failed before receiving a response"
  fail_json "$message" "SERVICE_UNAVAILABLE" "503"
fi
if [[ "$launch_status" != 2* ]]; then
  message="$(jq -r '.error.message? // .message? // empty' "$launch_file" 2>/dev/null || true)"
  error_code="$(jq -r '.error.code? // .code? // empty' "$launch_file" 2>/dev/null || true)"
  error_status="$(jq -r '.error.status? // .status? // empty' "$launch_file" 2>/dev/null || true)"
  [[ -n "$message" ]] || message="template launch failed ($launch_status)"
  [[ -n "$error_status" ]] || error_status="$launch_status"
  [[ -n "$error_code" ]] || error_code="$(platform_code_for_status "$error_status")"
  log_wrapper "launch failed status=$launch_status code=$error_code message=$message body=$(compact_response_body "$launch_file")"
  fail_json "$message" "$error_code" "$error_status"
fi

playground_id="$(jq -r '
  def first_object:
    if type == "array" then (.[0] // {})
    elif type == "object" then .
    else {}
    end;
  first_object
  | .id // .playground_id // (.playground | first_object | .id) // empty
' "$launch_file")"
[[ -n "$playground_id" ]] || fail_json "template launch did not return a playground id"
log_wrapper "playground_id=$playground_id wait_timeout=$wait_timeout"

wait_file="$(mktemp)"
if ! "$REAL_FIBE" --domain "$domain" --api-key "$api_key" --output "$output" \
  wait playground "$playground_id" --status running --timeout "$wait_timeout" --interval 8s >"$wait_file" 2>&1; then
  message="$(compact_response_body "$wait_file")"
  rm -f "$wait_file"
  [[ -n "$message" ]] || message="playground $playground_id did not reach running"
  log_wrapper "playground_id=$playground_id wait failed message=$message"
  fail_json "$message" "TIMEOUT" "408"
fi
rm -f "$wait_file"
log_wrapper "playground_id=$playground_id running"

jq -n --arg id "$playground_id" --arg name "$name" \
  '{playground:{id:($id|tonumber? // $id), name:$name}}'
