#!/usr/bin/env bash
set -euo pipefail

BUCKET="${1:-flyio-container-images}"
PREFIX="${2:-images/}"
REGION="${3:-}" # optional; if set, will be exported as AWS_REGION

if [[ -n "$REGION" ]]; then
  export AWS_REGION="$REGION"
fi

PASS=0
FAIL=0
WARN=0

say() { printf "%s\n" "$*"; }
ok()  { say "✓ $*"; }
err() { say "✗ $*"; }
inf() { say "• $*"; }

say "=== AWS S3 permission check ==="
say "Bucket: $BUCKET"
say "Prefix: $PREFIX"
if [[ -n "$REGION" ]]; then say "Region: $REGION"; fi
say ""

# 1) OPTIONAL: GetBucketLocation
if timeout 10 aws s3api get-bucket-location --bucket "$BUCKET" >/dev/null 2>&1; then
  ok "s3:GetBucketLocation"
else
  WARN=$((WARN+1))
  inf "s3:GetBucketLocation (optional): missing or denied"
fi

# 2) REQUIRED: ListObjectsV2
FIRST_KEY=""
if OUT=$(timeout 15 aws s3api list-objects-v2 --bucket "$BUCKET" --prefix "$PREFIX" --max-keys 1 2>&1); then
  ok "s3:ListBucket"
  FIRST_KEY=$(echo "$OUT" | jq -r '.Contents[0].Key // empty' 2>/dev/null || true)
  if [[ -n "$FIRST_KEY" ]]; then
    inf "sample key: $FIRST_KEY"
  else
    inf "no objects under prefix"
  fi
else
  FAIL=$((FAIL+1))
  err "s3:ListBucket -> $OUT"
fi

# 3) REQUIRED: HeadObject
if [[ -n "$FIRST_KEY" ]]; then
  if OUT=$(timeout 15 aws s3api head-object --bucket "$BUCKET" --key "$FIRST_KEY" 2>&1); then
    ok "s3:HeadObject"
  else
    FAIL=$((FAIL+1))
    err "s3:HeadObject -> $OUT"
  fi

  # 4) REQUIRED: GetObject (Range 0-0)
  TMP=$(mktemp)
  if OUT=$(timeout 20 aws s3api get-object --bucket "$BUCKET" --key "$FIRST_KEY" --range bytes=0-0 "$TMP" 2>&1); then
    ok "s3:GetObject (range 0-0)"
  else
    FAIL=$((FAIL+1))
    err "s3:GetObject -> $OUT"
  fi
  rm -f "$TMP"
fi

say ""
if [[ $FAIL -gt 0 ]]; then
  err "Result: $FAIL required permission(s) missing"
  exit 1
fi
ok "Result: all required permissions present"