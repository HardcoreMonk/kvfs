#!/usr/bin/env bash
set -euo pipefail

endpoint="${KVFS_S3_ENDPOINT:-http://127.0.0.1:8000}"
region="${KVFS_S3_REGION:-us-east-1}"
bucket="${KVFS_S3_BUCKET:-kvfs-smoke-$(date +%s)-$$}"
key="${KVFS_S3_KEY:-smoke.txt}"
expect_foundation="${KVFS_S3_EXPECT_FOUNDATION:-0}"

if [[ -z "${AWS_ACCESS_KEY_ID:-}" || -z "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
  echo "SKIP: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for SigV4 smoke" >&2
  exit 0
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "SKIP: aws CLI not installed" >&2
  exit 0
fi

if ! command -v mc >/dev/null 2>&1; then
  echo "SKIP: mc not installed" >&2
  exit 0
fi

tmpdir="$(mktemp -d)"
cleanup() {
  if [[ "$expect_foundation" != "1" ]]; then
    aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-object --bucket "$bucket" --key "$key" >/dev/null 2>&1 || true
    aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT
mkdir -p "$tmpdir/mc"
printf 'kvfs-s3-smoke\n' >"$tmpdir/payload.txt"

echo "[aws] list-buckets against ${endpoint}"
set +e
aws --endpoint-url "$endpoint" --region "$region" --output json s3api list-buckets >"$tmpdir/aws.out" 2>"$tmpdir/aws.err"
aws_status=$?
set -e

if [[ "$expect_foundation" == "1" ]]; then
  if [[ "$aws_status" -eq 0 ]]; then
    echo "FAIL: foundation mode expected aws list-buckets to fail with NotImplemented" >&2
    exit 1
  fi
  if ! grep -qi "NotImplemented" "$tmpdir/aws.err"; then
    echo "FAIL: expected aws stderr to mention NotImplemented" >&2
    cat "$tmpdir/aws.err" >&2
    exit 1
  fi
  echo "PASS: aws CLI reached S3 foundation and received NotImplemented"
  exit 0
fi

if [[ "$aws_status" -ne 0 ]]; then
  cat "$tmpdir/aws.err" >&2
  exit "$aws_status"
fi
echo "PASS: aws list-buckets succeeded"

echo "[aws] bucket/object workflow ${bucket}/${key}"
aws --endpoint-url "$endpoint" --region "$region" --output json s3api create-bucket --bucket "$bucket" >/dev/null
aws --endpoint-url "$endpoint" --region "$region" --output json s3api put-object --bucket "$bucket" --key "$key" --body "$tmpdir/payload.txt" >"$tmpdir/put.json"
aws --endpoint-url "$endpoint" --region "$region" --output json s3api head-object --bucket "$bucket" --key "$key" >"$tmpdir/head.json"
aws --endpoint-url "$endpoint" --region "$region" --output json s3api get-object --bucket "$bucket" --key "$key" "$tmpdir/download.txt" >"$tmpdir/get.json"
cmp "$tmpdir/payload.txt" "$tmpdir/download.txt"
aws --endpoint-url "$endpoint" --region "$region" --output json s3api list-objects-v2 --bucket "$bucket" --prefix smoke >"$tmpdir/list.json"
grep -q "\"Key\": \"$key\"" "$tmpdir/list.json"
echo "PASS: aws bucket/object workflow succeeded"

echo "[mc] alias + ls against ${endpoint}"
mc --config-dir "$tmpdir/mc" alias set kvfs-smoke "$endpoint" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null
mc --config-dir "$tmpdir/mc" ls kvfs-smoke/"$bucket" >"$tmpdir/mc.out"
grep -q "$key" "$tmpdir/mc.out"
echo "PASS: mc ls succeeded"

aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-object --bucket "$bucket" --key "$key" >/dev/null
aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-bucket --bucket "$bucket" >/dev/null
echo "PASS: cleanup succeeded"
