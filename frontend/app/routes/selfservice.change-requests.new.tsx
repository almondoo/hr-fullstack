/**
 * /selfservice/change-requests/new — Submit a new change request.
 *
 * BFF action: POST /api/v1/selfservice/change-requests
 * Requires selfservice:write permission.
 */

import {
  data,
  redirect,
  Form,
  Link,
  useActionData,
  useNavigation,
  useLoaderData,
} from "react-router";
import type { ActionFunctionArgs, LoaderFunctionArgs, MetaFunction } from "react-router";
import { z } from "zod";
import { apiMe, apiSubmitChangeRequest } from "~/lib/api.server";
import type { MeResponse } from "~/lib/api.server";
import { AppLayout } from "~/components/AppLayout";

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

export const meta: MetaFunction = () => [
  { title: "変更申請を作成 | HR SaaS" },
];

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const TARGET_TYPES = [
  { value: "employee_profile", label: "従業員プロフィール" },
  { value: "emergency_contact", label: "緊急連絡先" },
  { value: "commute", label: "通勤情報" },
  { value: "bank_account", label: "口座情報" },
  { value: "dependents", label: "扶養家族" },
] as const;

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

const ChangeRequestSchema = z.object({
  target_type: z.enum([
    "employee_profile",
    "emergency_contact",
    "commute",
    "bank_account",
    "dependents",
  ], { errorMap: () => ({ message: "申請種別を選択してください" }) }),
  changes_text: z
    .string()
    .min(1, "変更内容を入力してください")
    .refine((v) => {
      try {
        JSON.parse(v);
        return true;
      } catch {
        return false;
      }
    }, "変更内容はJSON形式で入力してください (例: {\"last_name\": \"山田\"})"),
});

type FieldErrors = Partial<Record<"target_type" | "changes_text", string>>;

interface ActionData {
  fieldErrors?: FieldErrors;
  formError?: string;
}

// ---------------------------------------------------------------------------
// Loader (auth guard)
// ---------------------------------------------------------------------------

interface LoaderData {
  me: MeResponse;
}

export async function loader({ request }: LoaderFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");
  const meResult = await apiMe(incomingCookie);
  if (!meResult.ok || !meResult.data) {
    return redirect("/login");
  }
  const responseHeaders = new Headers();
  for (const cookie of meResult.setCookieHeaders) {
    responseHeaders.append("Set-Cookie", cookie);
  }
  return data<LoaderData>({ me: meResult.data }, { headers: responseHeaders });
}

// ---------------------------------------------------------------------------
// Action
// ---------------------------------------------------------------------------

export async function action({ request }: ActionFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");
  const formData = await request.formData();

  const raw = {
    target_type: formData.get("target_type"),
    changes_text: formData.get("changes_text"),
  };

  const result = ChangeRequestSchema.safeParse(raw);
  if (!result.success) {
    const fieldErrors: FieldErrors = {};
    for (const issue of result.error.issues) {
      const key = issue.path[0] as keyof FieldErrors;
      if (!fieldErrors[key]) fieldErrors[key] = issue.message;
    }
    return data<ActionData>({ fieldErrors }, { status: 422 });
  }

  // Auth guard
  const meResult = await apiMe(incomingCookie);
  if (!meResult.ok || !meResult.data) {
    return redirect("/login");
  }
  const me = meResult.data;

  const changes = JSON.parse(result.data.changes_text) as Record<string, unknown>;

  const apiResult = await apiSubmitChangeRequest(incomingCookie, {
    employee_id: me.user.id,
    target_type: result.data.target_type,
    changes,
  });

  const responseHeaders = new Headers();
  for (const cookie of [...meResult.setCookieHeaders, ...apiResult.setCookieHeaders]) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  if (apiResult.status === 403) {
    return data<ActionData>(
      { formError: "変更申請の作成には selfservice:write 権限が必要です。" },
      { status: 403 },
    );
  }

  if (!apiResult.ok) {
    return data<ActionData>(
      { formError: "申請の送信に失敗しました。しばらく経ってから再試行してください。" },
      { status: 500 },
    );
  }

  return redirect("/selfservice/change-requests", { headers: responseHeaders });
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function NewChangeRequestPage() {
  const { me } = useLoaderData<LoaderData>();
  const actionData = useActionData<ActionData>();
  const navigation = useNavigation();
  const isSubmitting = navigation.state === "submitting";

  const defaultChangesPlaceholder = `{
  "last_name": "山田",
  "first_name": "太郎"
}`;

  return (
    <AppLayout displayName={me.user.displayName}>
      {/* Breadcrumb */}
      <nav style={styles.breadcrumb} aria-label="パンくずナビ">
        <Link to="/selfservice/change-requests" style={styles.breadcrumbLink}>
          変更申請一覧
        </Link>
        <span aria-hidden="true" style={styles.breadcrumbSep}>›</span>
        <span style={styles.breadcrumbCurrent} aria-current="page">新規申請</span>
      </nav>

      <h1 style={styles.heading}>変更申請を作成</h1>

      <div style={styles.card}>
        <p style={styles.description}>
          プロフィールや連絡先などの変更を申請します。申請後、担当者が内容を確認して承認します。
        </p>

        {actionData?.formError && (
          <div role="alert" aria-live="assertive" style={styles.formError}>
            {actionData.formError}
          </div>
        )}

        <Form method="post" noValidate aria-label="変更申請フォーム">
          {/* target_type */}
          <div style={styles.field}>
            <label htmlFor="target_type" style={styles.label}>
              申請種別 <span aria-label="必須" style={styles.required}>*</span>
            </label>
            <select
              id="target_type"
              name="target_type"
              required
              style={styles.select}
              aria-describedby={
                actionData?.fieldErrors?.target_type ? "target-type-error" : undefined
              }
              aria-invalid={actionData?.fieldErrors?.target_type ? "true" : undefined}
              defaultValue=""
            >
              <option value="" disabled>選択してください</option>
              {TARGET_TYPES.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
            {actionData?.fieldErrors?.target_type && (
              <span id="target-type-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.target_type}
              </span>
            )}
          </div>

          {/* changes_text */}
          <div style={styles.field}>
            <label htmlFor="changes_text" style={styles.label}>
              変更内容（JSON形式）<span aria-label="必須" style={styles.required}>*</span>
            </label>
            <textarea
              id="changes_text"
              name="changes_text"
              rows={8}
              required
              style={styles.textarea}
              placeholder={defaultChangesPlaceholder}
              aria-describedby={
                actionData?.fieldErrors?.changes_text
                  ? "changes-error"
                  : "changes-hint"
              }
              aria-invalid={actionData?.fieldErrors?.changes_text ? "true" : undefined}
            />
            <span id="changes-hint" style={styles.hint}>
              変更したいフィールドをJSONオブジェクト形式で入力してください。
              例: <code>{"{ \"last_name\": \"山田\" }"}</code>
            </span>
            {actionData?.fieldErrors?.changes_text && (
              <span id="changes-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.changes_text}
              </span>
            )}
          </div>

          <div style={styles.actions}>
            <Link to="/selfservice/change-requests" style={styles.cancelBtn}>
              キャンセル
            </Link>
            <button
              type="submit"
              disabled={isSubmitting}
              style={{
                ...styles.submitBtn,
                opacity: isSubmitting ? 0.6 : 1,
                cursor: isSubmitting ? "not-allowed" : "pointer",
              }}
              aria-busy={isSubmitting}
            >
              {isSubmitting ? "送信中..." : "申請を提出"}
            </button>
          </div>
        </Form>
      </div>
    </AppLayout>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const styles = {
  breadcrumb: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    marginBottom: "1.25rem",
    fontSize: "0.875rem",
  },
  breadcrumbLink: {
    color: "#2563eb",
    textDecoration: "none",
  },
  breadcrumbSep: {
    color: "#9ca3af",
  },
  breadcrumbCurrent: {
    color: "#6b7280",
  },
  heading: {
    margin: "0 0 1.5rem",
    fontSize: "1.75rem",
    fontWeight: 700,
    color: "#1a1a1a",
  },
  card: {
    backgroundColor: "#ffffff",
    borderRadius: "8px",
    boxShadow: "0 1px 4px rgba(0,0,0,0.08)",
    padding: "1.75rem",
    maxWidth: "600px",
  },
  description: {
    margin: "0 0 1.5rem",
    fontSize: "0.9rem",
    color: "#6b7280",
    lineHeight: 1.6,
  },
  formError: {
    backgroundColor: "#fef2f2",
    border: "1px solid #fecaca",
    borderRadius: "6px",
    color: "#b91c1c",
    padding: "0.75rem 1rem",
    marginBottom: "1.25rem",
    fontSize: "0.9rem",
  },
  field: {
    display: "flex",
    flexDirection: "column" as const,
    marginBottom: "1.25rem",
  },
  label: {
    fontSize: "0.875rem",
    fontWeight: 500,
    color: "#333",
    marginBottom: "0.375rem",
  },
  required: {
    color: "#dc2626",
    marginLeft: "0.125rem",
  },
  select: {
    padding: "0.625rem 0.75rem",
    border: "1px solid #d1d5db",
    borderRadius: "6px",
    fontSize: "1rem",
    outline: "none",
    backgroundColor: "#ffffff",
    width: "100%",
    boxSizing: "border-box" as const,
  },
  textarea: {
    padding: "0.625rem 0.75rem",
    border: "1px solid #d1d5db",
    borderRadius: "6px",
    fontSize: "0.9rem",
    fontFamily: "ui-monospace, SFMono-Regular, monospace",
    outline: "none",
    width: "100%",
    resize: "vertical" as const,
    boxSizing: "border-box" as const,
    lineHeight: 1.5,
  },
  hint: {
    marginTop: "0.375rem",
    fontSize: "0.8rem",
    color: "#6b7280",
    lineHeight: 1.5,
  },
  fieldError: {
    marginTop: "0.25rem",
    fontSize: "0.8rem",
    color: "#dc2626",
  },
  actions: {
    display: "flex",
    gap: "0.75rem",
    marginTop: "0.5rem",
    flexWrap: "wrap" as const,
  },
  cancelBtn: {
    padding: "0.75rem 1.25rem",
    borderRadius: "6px",
    fontSize: "0.9375rem",
    color: "#374151",
    border: "1px solid #d1d5db",
    backgroundColor: "#ffffff",
    textDecoration: "none",
    display: "inline-block",
  },
  submitBtn: {
    padding: "0.75rem 1.5rem",
    backgroundColor: "#2563eb",
    color: "#ffffff",
    border: "none",
    borderRadius: "6px",
    fontSize: "0.9375rem",
    fontWeight: 600,
  },
} as const;
