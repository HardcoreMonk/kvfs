#!/usr/bin/env bash
set -euo pipefail

endpoint="${KVFS_S3_ENDPOINT:-http://127.0.0.1:8000}"
region="${KVFS_S3_REGION:-us-east-1}"
bucket="${KVFS_S3_BUCKET:-kvfs-smoke}"
expect_foundation="${KVFS_S3_EXPECT_FOUNDATION:-1}"

if [[ -z "${AWS_ACCESS_KEY_ID:-}" || -z "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
  echo "SKIP: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for SigV4 smoke" >&2
  exit 0
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "SKIP: aws CLI not installed" >&2
  exit 0
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

echo "[aws] list-buckets against ${endpoint}"
set +e
aws --endpoint-url "$endpoint" --region "$region" s3api list-buckets >"$tmpdir/aws.out" 2>"$tmpdir/aws.err"
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
else
  if [[ "$aws_status" -ne 0 ]]; then
    cat "$tmpdir/aws.err" >&2
    exit "$aws_status"
  fi
  echo "PASS: aws list-buckets succeeded"
fi

if ! command -v mc >/dev/null 2>&1; then
  echo "SKIP: mc not installed" >&2
  exit 0
fi

echo "[mc] alias + ls against ${endpoint}"
mc alias set kvfs-smoke "$endpoint" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null
set +e
mc ls kvfs-smoke/"$bucket" >"$tmpdir/mc.out" 2>"$tmpdir/mc.err"
mc_status=$?
set -e

if [[ "$expect_foundation" == "1" ]]; then
  if [[ "$mc_status" -eq 0 ]]; then
    echo "FAIL: foundation mode expected mc ls to fail before P9-03" >&2
    exit 1
  fi
  echo "PASS: mc can target the endpoint; successful bucket behavior starts in P9-03"
else
  if [[ "$mc_status" -ne 0 ]]; then
    cat "$tmpdir/mc.err" >&2
    exit "$mc_status"
  fi
  echo "PASS: mc ls succeeded"
fi
