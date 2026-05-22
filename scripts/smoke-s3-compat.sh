#!/usr/bin/env bash
set -euo pipefail

endpoint="${KVFS_S3_ENDPOINT:-http://127.0.0.1:8000}"
region="${KVFS_S3_REGION:-us-east-1}"
bucket="${KVFS_S3_BUCKET:-kvfs-smoke-$(date +%s)-$$}"
key="${KVFS_S3_KEY:-smoke.txt}"
multipart_key="${KVFS_S3_MULTIPART_KEY:-multipart-smoke.txt}"
abort_key="${KVFS_S3_ABORT_KEY:-multipart-abort-smoke.txt}"
expect_foundation="${KVFS_S3_EXPECT_FOUNDATION:-0}"
multipart_upload_id=""
abort_upload_id=""

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
    if [[ -n "$multipart_upload_id" ]]; then
      aws --endpoint-url "$endpoint" --region "$region" --output json s3api abort-multipart-upload --bucket "$bucket" --key "$multipart_key" --upload-id "$multipart_upload_id" >/dev/null 2>&1 || true
    fi
    if [[ -n "$abort_upload_id" ]]; then
      aws --endpoint-url "$endpoint" --region "$region" --output json s3api abort-multipart-upload --bucket "$bucket" --key "$abort_key" --upload-id "$abort_upload_id" >/dev/null 2>&1 || true
    fi
    aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-object --bucket "$bucket" --key "$multipart_key" >/dev/null 2>&1 || true
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

printf 'kvfs-multipart-part-1\n' >"$tmpdir/part1.txt"
printf 'kvfs-multipart-part-2\n' >"$tmpdir/part2.txt"
cat "$tmpdir/part1.txt" "$tmpdir/part2.txt" >"$tmpdir/multipart-expected.txt"

echo "[aws] multipart workflow ${bucket}/${multipart_key}"
multipart_upload_id="$(aws --endpoint-url "$endpoint" --region "$region" --output text s3api create-multipart-upload --bucket "$bucket" --key "$multipart_key" --content-type text/plain --query UploadId)"
etag1_json="$(aws --endpoint-url "$endpoint" --region "$region" --output json s3api upload-part --bucket "$bucket" --key "$multipart_key" --upload-id "$multipart_upload_id" --part-number 1 --body "$tmpdir/part1.txt" --query ETag)"
etag2_json="$(aws --endpoint-url "$endpoint" --region "$region" --output json s3api upload-part --bucket "$bucket" --key "$multipart_key" --upload-id "$multipart_upload_id" --part-number 2 --body "$tmpdir/part2.txt" --query ETag)"
cat >"$tmpdir/complete-multipart.json" <<JSON
{"Parts":[{"ETag":${etag1_json},"PartNumber":1},{"ETag":${etag2_json},"PartNumber":2}]}
JSON
aws --endpoint-url "$endpoint" --region "$region" --output json s3api list-parts --bucket "$bucket" --key "$multipart_key" --upload-id "$multipart_upload_id" >"$tmpdir/list-parts.json"
grep -q '"PartNumber": 1' "$tmpdir/list-parts.json"
grep -q '"PartNumber": 2' "$tmpdir/list-parts.json"
aws --endpoint-url "$endpoint" --region "$region" --output json s3api complete-multipart-upload --bucket "$bucket" --key "$multipart_key" --upload-id "$multipart_upload_id" --multipart-upload "file://$tmpdir/complete-multipart.json" >"$tmpdir/complete-multipart.out"
multipart_upload_id=""
aws --endpoint-url "$endpoint" --region "$region" --output json s3api get-object --bucket "$bucket" --key "$multipart_key" "$tmpdir/multipart-download.txt" >"$tmpdir/multipart-get.json"
cmp "$tmpdir/multipart-expected.txt" "$tmpdir/multipart-download.txt"
echo "PASS: aws multipart complete workflow succeeded"

echo "[aws] multipart abort ${bucket}/${abort_key}"
abort_upload_id="$(aws --endpoint-url "$endpoint" --region "$region" --output text s3api create-multipart-upload --bucket "$bucket" --key "$abort_key" --query UploadId)"
aws --endpoint-url "$endpoint" --region "$region" --output json s3api abort-multipart-upload --bucket "$bucket" --key "$abort_key" --upload-id "$abort_upload_id" >/dev/null
abort_upload_id=""
echo "PASS: aws multipart abort workflow succeeded"

echo "[mc] alias + ls against ${endpoint}"
mc --config-dir "$tmpdir/mc" alias set kvfs-smoke "$endpoint" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null
mc --config-dir "$tmpdir/mc" ls kvfs-smoke/"$bucket" >"$tmpdir/mc.out"
grep -q "$key" "$tmpdir/mc.out"
echo "PASS: mc ls succeeded"

aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-object --bucket "$bucket" --key "$key" >/dev/null
aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-object --bucket "$bucket" --key "$multipart_key" >/dev/null
aws --endpoint-url "$endpoint" --region "$region" --output json s3api delete-bucket --bucket "$bucket" >/dev/null
echo "PASS: cleanup succeeded"
