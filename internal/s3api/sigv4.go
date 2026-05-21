// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	algorithmAWS4HMACSHA256 = "AWS4-HMAC-SHA256"
	emptyPayloadSHA256      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	maxClockSkew            = 15 * time.Minute
)

type Credentials struct {
	AccessKey string
	SecretKey string
}

type CredentialProvider interface {
	LookupAccessKey(ctx context.Context, accessKey string) (Credentials, bool)
}

type StaticCredentials map[string]string

func (s StaticCredentials) LookupAccessKey(_ context.Context, accessKey string) (Credentials, bool) {
	secret, ok := s[accessKey]
	if !ok {
		return Credentials{}, false
	}
	return Credentials{AccessKey: accessKey, SecretKey: secret}, true
}

type AuthResult struct {
	AccessKey     string
	Region        string
	Service       string
	SignedHeaders []string
	RequestTime   time.Time
}

func VerifyRequest(r *http.Request, provider CredentialProvider, now time.Time) (*AuthResult, error) {
	if provider == nil {
		return nil, NewError(http.StatusForbidden, CodeAccessDenied, "S3 credentials are not configured", r.URL.Path)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, NewError(http.StatusForbidden, CodeMissingSecurityHeader, "missing Authorization header", r.URL.Path)
	}
	parsed, err := parseAuthorization(auth)
	if err != nil {
		return nil, err
	}
	if parsed.algorithm != algorithmAWS4HMACSHA256 {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "unsupported authorization algorithm", r.URL.Path)
	}
	if len(parsed.scope) != 4 || parsed.scope[3] != "aws4_request" {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "credential scope must be date/region/service/aws4_request", r.URL.Path)
	}
	creds, ok := provider.LookupAccessKey(r.Context(), parsed.accessKey)
	if !ok {
		return nil, NewError(http.StatusForbidden, CodeAccessDenied, "unknown access key", r.URL.Path)
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return nil, NewError(http.StatusBadRequest, CodeMissingSecurityHeader, "missing X-Amz-Date header", r.URL.Path)
	}
	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid X-Amz-Date header", r.URL.Path)
	}
	if !validScopeDate(parsed.scope[0]) {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "credential scope date must be YYYYMMDD", r.URL.Path)
	}
	if parsed.scope[0] != reqTime.Format("20060102") {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "credential scope date must match X-Amz-Date", r.URL.Path)
	}
	if reqTime.Before(now.Add(-maxClockSkew)) || reqTime.After(now.Add(maxClockSkew)) {
		return nil, NewError(http.StatusForbidden, CodeAccessDenied, "request time is outside the allowed clock skew", r.URL.Path)
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		return nil, NewError(http.StatusBadRequest, CodeMissingSecurityHeader, "missing X-Amz-Content-Sha256 header", r.URL.Path)
	}
	if err := validateSignedHeaders(r, parsed.signedHeaders); err != nil {
		return nil, err
	}
	creq, err := canonicalRequest(r, parsed.signedHeaders, payloadHash)
	if err != nil {
		return nil, err
	}
	scope := strings.Join(parsed.scope, "/")
	sts := stringToSign(amzDate, scope, creq)
	key := signingKey(creds.SecretKey, parsed.scope[0], parsed.scope[1], parsed.scope[2])
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sts))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parsed.signature)) {
		return nil, NewError(http.StatusForbidden, CodeSignatureDoesNotMatch, "signature does not match", r.URL.Path)
	}
	return &AuthResult{
		AccessKey:     parsed.accessKey,
		Region:        parsed.scope[1],
		Service:       parsed.scope[2],
		SignedHeaders: parsed.signedHeaders,
		RequestTime:   reqTime,
	}, nil
}

func verifyOnly(r *http.Request, provider CredentialProvider, now time.Time) error {
	_, err := VerifyRequest(r, provider, now)
	return err
}

func validScopeDate(scopeDate string) bool {
	if len(scopeDate) != len("20060102") {
		return false
	}
	t, err := time.Parse("20060102", scopeDate)
	return err == nil && t.Format("20060102") == scopeDate
}

func validateSignedHeaders(r *http.Request, signedHeaders []string) error {
	signed := make(map[string]bool, len(signedHeaders))
	for _, name := range signedHeaders {
		signed[name] = true
	}
	for _, required := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
		if !signed[required] {
			return NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "SignedHeaders must include "+required, r.URL.Path)
		}
	}
	for name := range r.Header {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "x-amz-") && !signed[name] {
			return NewError(http.StatusForbidden, CodeAccessDenied, "x-amz headers must be signed", r.URL.Path)
		}
	}
	return nil
}

type parsedAuthorization struct {
	algorithm     string
	accessKey     string
	scope         []string
	signedHeaders []string
	signature     string
}

func parseAuthorization(auth string) (parsedAuthorization, error) {
	algo, rest, ok := strings.Cut(strings.TrimSpace(auth), " ")
	if !ok {
		return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid Authorization header", "")
	}
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid Authorization parameter", "")
		}
		fields[k] = v
	}
	credential := fields["Credential"]
	signedHeaders := fields["SignedHeaders"]
	signature := fields["Signature"]
	if credential == "" || signedHeaders == "" || signature == "" {
		return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "Authorization header requires Credential, SignedHeaders, and Signature", "")
	}
	accessKey, scopeRaw, ok := strings.Cut(credential, "/")
	if !ok || accessKey == "" {
		return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid Credential parameter", "")
	}
	scope := strings.Split(scopeRaw, "/")
	headers := strings.Split(strings.ToLower(signedHeaders), ";")
	sort.Strings(headers)
	return parsedAuthorization{algorithm: algo, accessKey: accessKey, scope: scope, signedHeaders: headers, signature: signature}, nil
}

func canonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	headers, signed, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.EscapedPath()),
		canonicalQuery(r.URL.RawQuery),
		headers,
		signed,
		payloadHash,
	}, "\n"), nil
}

func canonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return escapedPath
	}
	parts := strings.Split(decoded, "/")
	for i := range parts {
		parts[i] = sigv4Escape(parts[i])
	}
	return strings.Join(parts, "/")
}

func canonicalQuery(rawQuery string) string {
	type pair struct{ key, value string }
	var pairs []pair
	if rawQuery == "" {
		return ""
	}
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, pair{sigv4Escape(pathUnescape(key)), sigv4Escape(pathUnescape(value))})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.key+"="+p.value)
	}
	return strings.Join(out, "&")
}

func pathUnescape(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

func canonicalHeaders(r *http.Request, signedHeaders []string) (string, string, error) {
	if len(signedHeaders) == 0 {
		return "", "", NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "SignedHeaders must not be empty", r.URL.Path)
	}
	headers := make([]string, len(signedHeaders))
	copy(headers, signedHeaders)
	sort.Strings(headers)
	lines := make([]string, 0, len(headers)+1)
	for _, name := range headers {
		value := ""
		if name == "host" {
			value = r.Host
		} else {
			value = canonicalHeaderValue(r.Header.Values(name))
		}
		if value == "" {
			return "", "", NewError(http.StatusBadRequest, CodeMissingSecurityHeader, "missing signed header "+name, r.URL.Path)
		}
		lines = append(lines, name+":"+strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(headers, ";"), nil
}

func canonicalHeaderValue(values []string) string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

func sigv4Escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func stringToSign(amzDate, scope, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{
		algorithmAWS4HMACSHA256,
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

func signingKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}
