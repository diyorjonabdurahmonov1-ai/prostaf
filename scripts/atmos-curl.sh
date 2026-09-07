#!/usr/bin/env bash
#
# Prints the exact requests this backend sends to Atmos, filled in from .env, as
# curl commands you can paste into a terminal or into a message to Atmos support.
#
# # Why this exists
#
# When a payment is refused, the only two questions worth asking are "what did
# we send" and "what did they answer" — and neither is answerable from the logs,
# because the one place a request body is built (internal/atmos/atmos.go, post)
# deliberately never logs one. It carries card numbers.
#
# So this reconstructs the bodies instead, from the same .env the server reads,
# with the card fields left as obvious placeholders. Nothing here is guessed:
# every URL, header and field below is what internal/atmos/atmos.go and mps.go
# actually put on the wire, in the order Go's json.Marshal emits them — which is
# alphabetical by key, because the payloads are maps.
#
# Usage:
#   bash scripts/atmos-curl.sh              # local .env
#   bash scripts/atmos-curl.sh /path/to/.env
#
# The output contains live credentials. It is fine to run and read; think before
# pasting it anywhere public.
#
# Note that Atmos only answers from an IP registered in the merchant cabinet.
# From anywhere else these commands hang and then time out — that is the
# allowlist, not a bad key. A wrong key comes back as an immediate HTTP 401.

set -euo pipefail

ENV_FILE="${1:-$(dirname "$0")/../.env}"
if [ ! -f "$ENV_FILE" ]; then
  echo "no .env at $ENV_FILE" >&2
  exit 1
fi

# Read the file rather than sourcing it: values here contain characters a shell
# would happily interpret, and one stray backtick in a secret should not run.
get() {
  sed -n "s/^$1=//p" "$ENV_FILE" | head -n 1 | tr -d '\r'
}

BASE=$(get ATMOS_BASE_URL)
KEY=$(get ATMOS_CONSUMER_KEY)
SECRET=$(get ATMOS_CONSUMER_SECRET)
STORE=$(get ATMOS_STORE_ID)
TERMINAL=$(get ATMOS_TERMINAL_ID)
MPS_KEY=$(get ATMOS_MPS_API_KEY)
MPS_STORE=$(get ATMOS_MPS_STORE_ID)
: "${MPS_STORE:=$STORE}"

BASIC=$(printf '%s:%s' "$KEY" "$SECRET" | base64 | tr -d '\n')

# Fetch a real bearer token, so the commands below are paste-and-run rather than
# paste-then-fill-in-a-variable.
#
# This only works from an address registered in the merchant cabinet — from
# anywhere else apigw.atmos.uz accepts the connection and then never answers, so
# the request is capped at 25s and the script falls back to leaving $TOKEN in
# place. That fallback is not a failure worth stopping for: the request bodies
# are the point of this script, and they are the same either way.
#
# Set ATMOS_TOKEN in the environment to skip the fetch and use one you already
# have.
TOKEN_LITERAL="\$TOKEN"
TOKEN_NOTE=""
if [ -n "${ATMOS_TOKEN:-}" ]; then
  TOKEN_LITERAL="$ATMOS_TOKEN"
  TOKEN_NOTE="# Bearer token supplied via ATMOS_TOKEN."
else
  TOKEN_JSON=$(curl -s --max-time 25 -X POST "$BASE/token" \
    -H "Authorization: Basic $BASIC" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data 'grant_type=client_credentials' 2>/dev/null || true)
  # Pulled out with sed rather than a JSON parser: this has to run on a bare
  # server where jq and python are not promised.
  FETCHED=$(printf '%s' "$TOKEN_JSON" |
    sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  if [ -n "$FETCHED" ]; then
    TOKEN_LITERAL="$FETCHED"
    TOKEN_NOTE="# Bearer token below is REAL and was fetched $(date -u '+%Y-%m-%d %H:%M UTC').
# Atmos tokens last about an hour — rerun this script if the calls start
# answering HTTP 401."
  else
    TOKEN_NOTE="# COULD NOT FETCH A TOKEN from this machine, so the calls below still say
# \$TOKEN. Atmos answers only from an IP registered in the merchant cabinet;
# from anywhere else the request times out rather than being refused. Run this
# script ON THE SERVER to get the token filled in.
#
# What came back (empty means the connection timed out):
#   ${TOKEN_JSON:-<nothing>}"
  fi
fi

# Stand-ins, so the shape is complete without inventing a real card.
TXN="${TXN:-257000}"
PAYMENT_ID="${PAYMENT_ID:-00000000-0000-0000-0000-000000000000}"
AMOUNT_SOM="${AMOUNT_SOM:-1000}"
AMOUNT_TIYIN=$((AMOUNT_SOM * 100))

# terminal_id is sent only by accounts that were issued one — see
# CreateTransaction. Omitted entirely rather than sent empty.
#
# Placed after store_id, not before it, because Go marshals a map in sorted key
# order and that is what actually goes on the wire — the point of this script is
# to be comparable against Atmos's logs byte for byte.
TERMINAL_LINE=""
if [ -n "$TERMINAL" ]; then
  TERMINAL_LINE=",
  \"terminal_id\": \"$TERMINAL\""
fi

cat <<EOF
################################################################################
# 1. Token — every call below needs one. Cached until it expires; refetched once
#    on a 401. (atmos.go: accessToken)
################################################################################

curl -i -X POST '$BASE/token' \\
  -H 'Authorization: Basic $BASIC' \\
  -H 'Content-Type: application/x-www-form-urlencoded' \\
  --data 'grant_type=client_credentials'

$TOKEN_NOTE

################################################################################
# 2. Open a transaction — sent when a top-up or a subscription is started.
#    amount is in TIYIN; account is our own payment row id. (CreateTransaction)
################################################################################

curl -i -X POST '$BASE/merchant/pay/create' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'Content-Type: application/json' \\
  -d '{
  "account": "$PAYMENT_ID",
  "amount": $AMOUNT_TIYIN,
  "lang": "uz",
  "store_id": $STORE$TERMINAL_LINE
}'

################################################################################
# 3. Hand over the card — UzCard / Humo only. Atmos texts the cardholder a code.
#    Nothing is debited here. expiry is YYmm, NOT MMYY. (PreApply)
#
#    This is the call that answers "Karta topilmadi" / "Карта не найдена"
#    (STPIMS-ERR-004) for a card the store's processing does not hold.
################################################################################

curl -i -X POST '$BASE/merchant/pay/pre-apply' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'Content-Type: application/json' \\
  -d '{
  "card_number": "8600XXXXXXXXXXXX",
  "expiry": "2803",
  "store_id": $STORE,
  "transaction_id": $TXN
}'

################################################################################
# 4. Confirm with the SMS code. THIS is the call that moves the money.
#    otp is a STRING — as a number, "012345" becomes 12345 and is refused.
################################################################################

curl -i -X POST '$BASE/merchant/pay/apply' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'Content-Type: application/json' \\
  -d '{
  "otp": "111111",
  "store_id": $STORE,
  "transaction_id": $TXN
}'

################################################################################
# 5. Resend the code. (RetryOTP)
################################################################################

curl -i -X POST '$BASE/merchant/pay/retry-otp' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'Content-Type: application/json' \\
  -d '{
  "store_id": $STORE,
  "transaction_id": $TXN
}'
EOF

if [ -z "$MPS_KEY" ]; then
  cat <<'EOF'

################################################################################
# Visa / Mastercard (MPS) — NOT SENT, because ATMOS_MPS_API_KEY is empty.
#
# International cards go through a separate module with a key of its own. While
# it is unset the server refuses those cards up front with a sentence saying so,
# rather than sending them to /merchant/pay/pre-apply, which would answer
# "Karta topilmadi" about a card that is perfectly good. Set the key and rerun
# this script to see the three calls filled in.
################################################################################
EOF
  exit 0
fi

cat <<EOF

################################################################################
# 6. Visa / Mastercard. A different module: extra 'apikey' header, its own
#    three calls, a CVC2, and expiry the other way round — MMYY, not YYmm.
################################################################################

# 6a. Draft for the amount. (MPSPreCreate)
curl -i -X POST '$BASE/mps/pay/transaction/pre-create' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'apikey: $MPS_KEY' \\
  -H 'Content-Type: application/json' \\
  -d '{
  "account": "$PAYMENT_ID",
  "amount": $AMOUNT_TIYIN,
  "ext_id": "$PAYMENT_ID",
  "invoice_id": "$PAYMENT_ID",
  "ofd_items": [],
  "store_id": $MPS_STORE
}'

# 6b. Authorise the card. Returns redirect_uri — the payer's own bank's 3DS
#     page. Nothing is captured yet. (MPSCreate)
curl -i -X POST '$BASE/mps/pay/transaction/create' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'apikey: $MPS_KEY' \\
  -H 'Content-Type: application/json' \\
  -d '{
  "card_name": "CARD",
  "client_ip_addr": "1.2.3.4",
  "cvc2": "000",
  "expiry": "0328",
  "ext_id": "$PAYMENT_ID",
  "pan": "4XXXXXXXXXXXXXXX",
  "transaction_id": $TXN
}'

# 6c. Capture, after the payer has cleared 3DS. THIS moves the money.
#     (MPSApply)
curl -i -X POST '$BASE/mps/pay/transaction/apply' \\
  -H "Authorization: Bearer $TOKEN_LITERAL" \\
  -H 'apikey: $MPS_KEY' \\
  -H 'Content-Type: application/json' \\
  -d '{
  "transaction_id": $TXN
}'
EOF
