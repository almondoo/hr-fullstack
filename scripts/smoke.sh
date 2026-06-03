#!/usr/bin/env sh
# scripts/smoke.sh — HR SaaS compose smoke test
#
# Usage:
#   ./scripts/smoke.sh [API_BASE_URL]
#
# Default: http://localhost:18080
#   (uses port 18080 to avoid conflict with other services on 8080)
#
# Prerequisites:
#   - docker compose up (db + api running)
#   - curl available in PATH
#
# What it checks:
#   1. GET /healthz  → 200
#   2. GET /readyz   → 200
#   3. GET /api/v1/csrf → 200 + csrf_token field
#   4. POST /api/v1/auth/signup → 201 (creates smoke-test tenant)
#   5. POST /api/v1/auth/login  → 200
#   6. GET  /api/v1/auth/me     → 200 + correct user_id
#   7. POST /api/v1/employees   → 201
#   8. GET  /api/v1/employees   → 200, contains created employee
#   9. RLS isolation: second tenant sees 0 employees (cross-tenant leak check)
#
# Exit code: 0 if all checks pass, 1 on first failure.

set -e

BASE="${1:-http://localhost:18080}"
ORIGIN="http://localhost:3000"
PASS="SmokeP@ss1!"
COOKIE_A="$(mktemp)"
COOKIE_B="$(mktemp)"
SLUG_A="smoke-tenant-$(date +%s)"
SLUG_B="smoke-tenant-b-$(date +%s)"

fail() {
    echo "FAIL: $1" >&2
    rm -f "$COOKIE_A" "$COOKIE_B"
    exit 1
}

pass() {
    echo "PASS: $1"
}

check_status() {
    EXPECTED="$1"
    ACTUAL="$2"
    MSG="$3"
    if [ "$ACTUAL" != "$EXPECTED" ]; then
        fail "$MSG (expected HTTP $EXPECTED, got $ACTUAL)"
    fi
    pass "$MSG"
}

echo "=== HR SaaS Smoke Test ==="
echo "Target: $BASE"
echo ""

# -----------------------------------------------------------------
# 1. /healthz
# -----------------------------------------------------------------
STATUS=$(curl -so /dev/null -w "%{http_code}" "$BASE/healthz")
check_status "200" "$STATUS" "/healthz → 200"

# -----------------------------------------------------------------
# 2. /readyz
# -----------------------------------------------------------------
STATUS=$(curl -so /dev/null -w "%{http_code}" "$BASE/readyz")
check_status "200" "$STATUS" "/readyz → 200"

# -----------------------------------------------------------------
# 3. CSRF token (Tenant A)
# -----------------------------------------------------------------
CSRF_RESP=$(curl -s -c "$COOKIE_A" "$BASE/api/v1/csrf")
CSRF_A=$(echo "$CSRF_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('csrf_token',''))" 2>/dev/null)
if [ -z "$CSRF_A" ]; then
    fail "GET /api/v1/csrf did not return csrf_token"
fi
pass "GET /api/v1/csrf → token obtained"

# -----------------------------------------------------------------
# 4. Signup (Tenant A)
# -----------------------------------------------------------------
SIGNUP_RESP=$(curl -s -w "\n%{http_code}" -b "$COOKIE_A" -c "$COOKIE_A" \
    -X POST "$BASE/api/v1/auth/signup" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_A" \
    -H "Origin: $ORIGIN" \
    -d "{\"email\":\"admin@$SLUG_A.example\",\"password\":\"$PASS\",\"tenant_name\":\"Smoke Tenant A\",\"slug\":\"$SLUG_A\"}")
SIGNUP_BODY=$(echo "$SIGNUP_RESP" | head -1)
SIGNUP_STATUS=$(echo "$SIGNUP_RESP" | tail -1)
check_status "201" "$SIGNUP_STATUS" "POST /api/v1/auth/signup (Tenant A) → 201"

# -----------------------------------------------------------------
# 5. Login (Tenant A)
# -----------------------------------------------------------------
CSRF_A=$(curl -s -b "$COOKIE_A" -c "$COOKIE_A" "$BASE/api/v1/csrf" | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('csrf_token',''))" 2>/dev/null)
LOGIN_RESP=$(curl -s -w "\n%{http_code}" -b "$COOKIE_A" -c "$COOKIE_A" \
    -X POST "$BASE/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_A" \
    -H "Origin: $ORIGIN" \
    -d "{\"email\":\"admin@$SLUG_A.example\",\"password\":\"$PASS\",\"slug\":\"$SLUG_A\"}")
LOGIN_BODY=$(echo "$LOGIN_RESP" | head -1)
LOGIN_STATUS=$(echo "$LOGIN_RESP" | tail -1)
check_status "200" "$LOGIN_STATUS" "POST /api/v1/auth/login (Tenant A) → 200"
USER_ID_A=$(echo "$LOGIN_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('user_id',''))" 2>/dev/null)

# -----------------------------------------------------------------
# 6. GET /auth/me
# -----------------------------------------------------------------
ME_RESP=$(curl -s -w "\n%{http_code}" -b "$COOKIE_A" \
    "$BASE/api/v1/auth/me" \
    -H "Origin: $ORIGIN")
ME_BODY=$(echo "$ME_RESP" | head -1)
ME_STATUS=$(echo "$ME_RESP" | tail -1)
check_status "200" "$ME_STATUS" "GET /api/v1/auth/me → 200"
ME_USER=$(echo "$ME_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('user_id',''))" 2>/dev/null)
if [ "$ME_USER" != "$USER_ID_A" ]; then
    fail "/auth/me returned user_id $ME_USER, expected $USER_ID_A"
fi
pass "/auth/me user_id matches login"

# -----------------------------------------------------------------
# 7. Create employee (Tenant A)
# -----------------------------------------------------------------
CSRF_A=$(curl -s -b "$COOKIE_A" -c "$COOKIE_A" "$BASE/api/v1/csrf" | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('csrf_token',''))" 2>/dev/null)
EMP_RESP=$(curl -s -w "\n%{http_code}" -b "$COOKIE_A" -c "$COOKIE_A" \
    -X POST "$BASE/api/v1/employees" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_A" \
    -H "Origin: $ORIGIN" \
    -d '{"employee_code":"SMOKE001","last_name":"Smoke","first_name":"User","employment_type":"full_time","status":"active"}')
EMP_BODY=$(echo "$EMP_RESP" | head -1)
EMP_STATUS=$(echo "$EMP_RESP" | tail -1)
check_status "201" "$EMP_STATUS" "POST /api/v1/employees (Tenant A) → 201"
EMP_ID=$(echo "$EMP_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)

# -----------------------------------------------------------------
# 8. List employees (Tenant A) — should contain created employee
# -----------------------------------------------------------------
LIST_RESP=$(curl -s -b "$COOKIE_A" "$BASE/api/v1/employees" -H "Origin: $ORIGIN")
EMP_COUNT=$(echo "$LIST_RESP" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('employees',[])))" 2>/dev/null)
if [ "$EMP_COUNT" -lt 1 ]; then
    fail "GET /api/v1/employees returned 0 employees after create (expected >= 1)"
fi
pass "GET /api/v1/employees (Tenant A) → $EMP_COUNT employee(s)"

# -----------------------------------------------------------------
# 9. RLS cross-tenant isolation: Tenant B sees 0 employees
# -----------------------------------------------------------------
CSRF_B=$(curl -s -c "$COOKIE_B" "$BASE/api/v1/csrf" | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('csrf_token',''))" 2>/dev/null)
curl -s -b "$COOKIE_B" -c "$COOKIE_B" \
    -X POST "$BASE/api/v1/auth/signup" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_B" \
    -H "Origin: $ORIGIN" \
    -d "{\"email\":\"admin@$SLUG_B.example\",\"password\":\"$PASS\",\"tenant_name\":\"Smoke Tenant B\",\"slug\":\"$SLUG_B\"}" > /dev/null

CSRF_B=$(curl -s -b "$COOKIE_B" -c "$COOKIE_B" "$BASE/api/v1/csrf" | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('csrf_token',''))" 2>/dev/null)
curl -s -b "$COOKIE_B" -c "$COOKIE_B" \
    -X POST "$BASE/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -H "X-CSRF-Token: $CSRF_B" \
    -H "Origin: $ORIGIN" \
    -d "{\"email\":\"admin@$SLUG_B.example\",\"password\":\"$PASS\",\"slug\":\"$SLUG_B\"}" > /dev/null

LIST_B=$(curl -s -b "$COOKIE_B" "$BASE/api/v1/employees" -H "Origin: $ORIGIN")
COUNT_B=$(echo "$LIST_B" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('employees',[])))" 2>/dev/null)
if [ "$COUNT_B" -ne 0 ]; then
    fail "RLS VIOLATION: Tenant B can see $COUNT_B employees belonging to Tenant A"
fi
pass "RLS isolation: Tenant B sees 0 employees (cross-tenant leak = none)"

# -----------------------------------------------------------------
# Done
# -----------------------------------------------------------------
rm -f "$COOKIE_A" "$COOKIE_B"
echo ""
echo "=== All smoke checks passed ==="
