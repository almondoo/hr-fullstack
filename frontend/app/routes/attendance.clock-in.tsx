/**
 * /attendance/clock-in — Submit a new attendance record (clock in/out).
 *
 * BFF action: POST /api/v1/attendance/records with CSRF token.
 * Requires attendance:write permission.
 */

import { data, redirect, Form, Link, useActionData, useNavigation, useLoaderData } from "react-router";
import type { ActionFunctionArgs, LoaderFunctionArgs, MetaFunction } from "react-router";
import { z } from "zod";
import { apiMe, apiCreateAttendanceRecord } from "~/lib/api.server";
import type { MeResponse } from "~/lib/api.server";
import { AppLayout } from "~/components/AppLayout";

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

export const meta: MetaFunction = () => [
  { title: "打刻 | HR SaaS" },
];

// ---------------------------------------------------------------------------
// Validation schema
// ---------------------------------------------------------------------------

const ClockInSchema = z.object({
  work_date: z
    .string()
    .regex(/^\d{4}-\d{2}-\d{2}$/, "日付はYYYY-MM-DD形式で入力してください"),
  clock_in: z
    .string()
    .optional()
    .refine((v) => !v || /^\d{2}:\d{2}$/.test(v), "HH:MM形式で入力してください"),
  clock_out: z
    .string()
    .optional()
    .refine((v) => !v || /^\d{2}:\d{2}$/.test(v), "HH:MM形式で入力してください"),
  break_minutes: z
    .string()
    .transform((v) => parseInt(v || "0", 10))
    .pipe(z.number().int().min(0, "休憩時間は0以上を入力してください")),
  note: z.string().max(1000, "備考は1000文字以内で入力してください").optional(),
});

type FieldErrors = Partial<
  Record<"work_date" | "clock_in" | "clock_out" | "break_minutes" | "note", string>
>;

interface ActionData {
  fieldErrors?: FieldErrors;
  formError?: string;
  success?: boolean;
}

// ---------------------------------------------------------------------------
// Loader (auth guard)
// ---------------------------------------------------------------------------

interface LoaderData {
  me: MeResponse;
  todayIso: string;
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
  const todayIso = new Date().toISOString().slice(0, 10);
  return data<LoaderData>({ me: meResult.data, todayIso }, { headers: responseHeaders });
}

// ---------------------------------------------------------------------------
// Action
// ---------------------------------------------------------------------------

export async function action({ request }: ActionFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");
  const formData = await request.formData();

  const raw = {
    work_date: formData.get("work_date"),
    clock_in: formData.get("clock_in") || undefined,
    clock_out: formData.get("clock_out") || undefined,
    break_minutes: formData.get("break_minutes"),
    note: formData.get("note") || undefined,
  };

  const result = ClockInSchema.safeParse(raw);
  if (!result.success) {
    const fieldErrors: FieldErrors = {};
    for (const issue of result.error.issues) {
      const key = issue.path[0] as keyof FieldErrors;
      if (!fieldErrors[key]) fieldErrors[key] = issue.message;
    }
    return data<ActionData>({ fieldErrors }, { status: 422 });
  }

  // Auth guard for the action
  const meResult = await apiMe(incomingCookie);
  if (!meResult.ok || !meResult.data) {
    const h = new Headers({ Location: "/login" });
    return redirect("/login", { headers: h });
  }
  const me = meResult.data;

  const { work_date, clock_in, clock_out, break_minutes, note } = result.data;

  // Build RFC3339 timestamps from local date + HH:MM time strings
  function toRFC3339(date: string, time: string): string {
    return `${date}T${time}:00+09:00`;
  }

  const apiResult = await apiCreateAttendanceRecord(incomingCookie, {
    employee_id: me.user.id,
    work_date,
    clock_in: clock_in ? toRFC3339(work_date, clock_in) : null,
    clock_out: clock_out ? toRFC3339(work_date, clock_out) : null,
    break_minutes,
    source: "web",
    note: note ?? null,
  });

  const responseHeaders = new Headers();
  for (const cookie of [...meResult.setCookieHeaders, ...apiResult.setCookieHeaders]) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  if (apiResult.status === 403) {
    return data<ActionData>(
      { formError: "勤怠記録の作成には attendance:write 権限が必要です。" },
      { status: 403 },
    );
  }

  if (apiResult.status === 409) {
    return data<ActionData>(
      { formError: "この日付の勤怠記録はすでに存在します。" },
      { status: 409 },
    );
  }

  if (!apiResult.ok) {
    return data<ActionData>(
      { formError: "打刻の記録に失敗しました。しばらく経ってから再試行してください。" },
      { status: 500 },
    );
  }

  return redirect("/attendance", { headers: responseHeaders });
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function ClockInPage() {
  const { me, todayIso } = useLoaderData<LoaderData>();
  const actionData = useActionData<ActionData>();
  const navigation = useNavigation();
  const isSubmitting = navigation.state === "submitting";

  return (
    <AppLayout displayName={me.user.displayName}>
      <div style={styles.breadcrumb} aria-label="パンくずナビ">
        <Link to="/attendance" style={styles.breadcrumbLink}>
          勤怠管理
        </Link>
        <span aria-hidden="true" style={styles.breadcrumbSep}>›</span>
        <span style={styles.breadcrumbCurrent} aria-current="page">打刻</span>
      </div>

      <h1 style={styles.heading}>打刻</h1>

      <div style={styles.card}>
        {actionData?.formError && (
          <div role="alert" aria-live="assertive" style={styles.formError}>
            {actionData.formError}
          </div>
        )}

        <Form method="post" noValidate aria-label="打刻フォーム">
          {/* work_date */}
          <div style={styles.field}>
            <label htmlFor="work_date" style={styles.label}>
              勤務日 <span aria-label="必須" style={styles.required}>*</span>
            </label>
            <input
              id="work_date"
              name="work_date"
              type="date"
              required
              defaultValue={todayIso}
              style={styles.input}
              aria-describedby={
                actionData?.fieldErrors?.work_date ? "work-date-error" : undefined
              }
              aria-invalid={actionData?.fieldErrors?.work_date ? "true" : undefined}
            />
            {actionData?.fieldErrors?.work_date && (
              <span id="work-date-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.work_date}
              </span>
            )}
          </div>

          {/* clock_in */}
          <div style={styles.field}>
            <label htmlFor="clock_in" style={styles.label}>
              出勤時刻
            </label>
            <input
              id="clock_in"
              name="clock_in"
              type="time"
              style={styles.input}
              aria-describedby={
                actionData?.fieldErrors?.clock_in ? "clock-in-error" : "clock-in-hint"
              }
              aria-invalid={actionData?.fieldErrors?.clock_in ? "true" : undefined}
            />
            <span id="clock-in-hint" style={styles.hint}>
              省略すると出勤時刻なしで記録されます
            </span>
            {actionData?.fieldErrors?.clock_in && (
              <span id="clock-in-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.clock_in}
              </span>
            )}
          </div>

          {/* clock_out */}
          <div style={styles.field}>
            <label htmlFor="clock_out" style={styles.label}>
              退勤時刻
            </label>
            <input
              id="clock_out"
              name="clock_out"
              type="time"
              style={styles.input}
              aria-describedby={
                actionData?.fieldErrors?.clock_out ? "clock-out-error" : undefined
              }
              aria-invalid={actionData?.fieldErrors?.clock_out ? "true" : undefined}
            />
            {actionData?.fieldErrors?.clock_out && (
              <span id="clock-out-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.clock_out}
              </span>
            )}
          </div>

          {/* break_minutes */}
          <div style={styles.field}>
            <label htmlFor="break_minutes" style={styles.label}>
              休憩時間（分）
            </label>
            <input
              id="break_minutes"
              name="break_minutes"
              type="number"
              min={0}
              defaultValue={0}
              style={{ ...styles.input, maxWidth: "140px" }}
              aria-describedby={
                actionData?.fieldErrors?.break_minutes ? "break-error" : undefined
              }
              aria-invalid={actionData?.fieldErrors?.break_minutes ? "true" : undefined}
            />
            {actionData?.fieldErrors?.break_minutes && (
              <span id="break-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.break_minutes}
              </span>
            )}
          </div>

          {/* note */}
          <div style={styles.field}>
            <label htmlFor="note" style={styles.label}>
              備考
            </label>
            <textarea
              id="note"
              name="note"
              rows={3}
              maxLength={1000}
              style={styles.textarea}
              aria-describedby={
                actionData?.fieldErrors?.note ? "note-error" : undefined
              }
              aria-invalid={actionData?.fieldErrors?.note ? "true" : undefined}
            />
            {actionData?.fieldErrors?.note && (
              <span id="note-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.note}
              </span>
            )}
          </div>

          <div style={styles.actions}>
            <Link to="/attendance" style={styles.cancelBtn}>
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
              {isSubmitting ? "記録中..." : "打刻を記録"}
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
    maxWidth: "540px",
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
  input: {
    padding: "0.625rem 0.75rem",
    border: "1px solid #d1d5db",
    borderRadius: "6px",
    fontSize: "1rem",
    outline: "none",
    width: "100%",
    boxSizing: "border-box" as const,
  },
  textarea: {
    padding: "0.625rem 0.75rem",
    border: "1px solid #d1d5db",
    borderRadius: "6px",
    fontSize: "1rem",
    outline: "none",
    width: "100%",
    resize: "vertical" as const,
    boxSizing: "border-box" as const,
  },
  hint: {
    marginTop: "0.25rem",
    fontSize: "0.8rem",
    color: "#6b7280",
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
