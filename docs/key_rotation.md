# Key Rotation Runbook

> Scope: field-level encryption keys for PII columns (マイナンバー, 口座番号, etc.)
> operated via `internal/platform/crypto`.  Session / CSRF keys are covered separately
> in the security checklist (`docs/04_tech_stack.md §5`).

---

## Key hierarchy

```
AWS KMS CMK (KEK — Key Encryption Key)
  └─ DEK (Data Encryption Key, 32 bytes AES-256)
       └─ PII ciphertext in database columns
```

| Layer | Where stored | Who touches it |
|-------|-------------|----------------|
| KEK (CMK) | AWS KMS HSM — never leaves | AWS managed |
| DEK plaintext | Process memory only — zeroed after `NewFieldCipher` | App at startup |
| DEK ciphertext (`EncryptedDataKey`) | Stored alongside each ciphertext record (future) | App + KMS |
| PII ciphertext | `encrypted_*` DB columns | App |

---

## Provider modes

### `KEY_PROVIDER=env` (default / development)

- Key material in `FIELD_ENCRYPTION_KEY` (base64-encoded 32-byte value).
- Suitable for development and for environments where a secrets manager injects
  the variable at container launch (e.g. AWS Secrets Manager → ECS task definition
  environment injection, GCP Secret Manager → Cloud Run secret volume).
- Rotation: generate a new key, re-encrypt all rows (see Rotation Procedure below),
  then update the secret in the secrets manager and redeploy.

### `KEY_PROVIDER=aws-kms` (production target)

- `KMS_KEY_ID` holds the CMK ARN or alias.
- At startup, `KMSKeyProvider.DataKey()` calls `kms.GenerateDataKey(AES_256)`,
  receiving a fresh 32-byte plaintext DEK and its KMS-encrypted blob.
- The plaintext DEK is used once (to build `FieldCipher`) then zeroed.
- The encrypted blob (`EncryptedDataKey`) **should** be stored alongside each
  encrypted column value for per-record re-keying (see § Future: per-record DEK).

---

## Rotation procedure (zero-downtime, dual-key)

The rotation strategy is **dual-key read / single-key write**: during the
transition window the application can decrypt with either the old or the new key,
but writes only with the new key.  This avoids a maintenance window.

### Step 1 — provision the new key

**`KEY_PROVIDER=env` path:**
```
# Generate a cryptographically random 32-byte key and base64-encode it.
# Use openssl or a secrets manager CLI — never commit the output.
openssl rand -base64 32
# Store the result in your secrets manager under a versioned name, e.g.:
#   aws secretsmanager put-secret-value \
#     --secret-id hr-saas/field-encryption-key \
#     --secret-string "$(openssl rand -base64 32)"
```

**`KEY_PROVIDER=aws-kms` path:**
```
# Create a new CMK version (KMS automatic rotation, annual):
aws kms enable-key-rotation --key-id alias/hr-saas-field-encryption
# KMS rotates the backing key material automatically; the CMK ARN is unchanged.
# No application deployment is needed for CMK backing-material rotation.
#
# For a full CMK replacement (new ARN), create a new CMK and update KMS_KEY_ID.
```

### Step 2 — re-encrypt existing rows

Run the re-encryption migration job (to be implemented as a background CLI command
under `cmd/rekey/`):

```
# Pseudocode — implement as a Go CLI or migration job:
for each row in tables with encrypted_* columns:
    plaintext = old_cipher.Decrypt(row.encrypted_value)
    row.encrypted_value = new_cipher.Encrypt(plaintext)
    zero(plaintext)
    db.Save(row)
```

The job should:
- Run with both old and new `FieldCipher` instances in memory.
- Process rows in batches with a configurable batch size to avoid lock contention.
- Be idempotent: skip rows already encrypted with the new key (detect via a
  `key_version` column — add this column before running).
- Be run **before** switching the application to the new key.

### Step 3 — switch to the new key

After all rows are re-encrypted, update the secret / environment variable and
perform a rolling restart of the application containers.  Because all rows are
already using the new key at this point, zero decryption failures are expected.

### Step 4 — retire the old key

After confirming zero decryption errors in logs for ≥ 24 hours:
- Remove the old key version from the secrets manager.
- (KMS path) Disable or schedule deletion of the old CMK version after the
  AWS-mandated 7–30 day waiting period.

---

## Rotation frequency

| Key | Recommended rotation frequency | Trigger |
|-----|--------------------------------|---------|
| DEK (`FIELD_ENCRYPTION_KEY`) | Annually or on suspected compromise | Ops calendar + incident response |
| CMK backing material (KMS) | Annually via AWS auto-rotation | `enable-key-rotation` flag |
| CMK (full ARN change) | On compromise or compliance requirement | Incident response |
| Session hash/block keys | Annually or on compromise | Ops calendar |
| CSRF auth key | Annually or on compromise | Ops calendar (invalidates active CSRF tokens) |

---

## Future: per-record DEK envelope (recommended for production)

The current `KMSKeyProvider` generates a single DEK per startup (stateless mode).
For stronger isolation, store `EncryptedDataKey` alongside each encrypted value:

```sql
-- Example column layout (add to migration):
ALTER TABLE employees
    ADD COLUMN my_number_ciphertext   BYTEA,
    ADD COLUMN my_number_encrypted_dek BYTEA;  -- KMS-wrapped DEK for this row
```

On write:
1. Call `kms.GenerateDataKey` → `(plaintext_dek, encrypted_dek)`.
2. Encrypt the field with `plaintext_dek`; store `encrypted_dek` in the column.
3. Zero `plaintext_dek`.

On read:
1. Call `kms.Decrypt(encrypted_dek)` → `plaintext_dek`.
2. Decrypt the field; zero `plaintext_dek`.

On rotation:
1. Call `kms.ReEncrypt(encrypted_dek, new_key_id)` → new `encrypted_dek` for each row.
2. No plaintext DEK touches the application; all re-keying happens inside KMS.

This pattern eliminates the need for a bulk re-encryption job and reduces the
blast radius of a single DEK compromise to one row.  Implement this in
`internal/platform/crypto/kms_keyprovider.go` once the schema migration for
per-record DEK columns is approved (issue #10, infra dependency GAP-01).

---

## Infra dependencies (pending — issue #9)

The following items require Terraform work in `infra/modules/secrets/` before
`KEY_PROVIDER=aws-kms` can be activated in staging/production:

- [ ] CMK created and ARN documented in `infra/modules/secrets/`.
- [ ] ECS task role IAM policy: `kms:GenerateDataKey` + `kms:Decrypt` on the CMK.
- [ ] `KMS_KEY_ID` wired into ECS task definition environment via Secrets Manager.
- [ ] CloudTrail / KMS key usage logging enabled for audit.
- [ ] Key rotation schedule set (`enable-key-rotation` or annual reminder).

Until these are in place, `KEY_PROVIDER=env` with AWS Secrets Manager injection
is the production path.
