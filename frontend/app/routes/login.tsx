import { data, redirect, Form, useActionData, useNavigation } from "react-router";
import type { ActionFunctionArgs, MetaFunction } from "react-router";
import { z } from "zod";
import { apiLogin } from "~/lib/api.server";

// ---------------------------------------------------------------------------
// Zod schema
// ---------------------------------------------------------------------------

const LoginSchema = z.object({
  slug: z
    .string()
    .min(1, "テナントIDを入力してください")
    .regex(/^[a-z0-9-]+$/, "テナントIDは英小文字・数字・ハイフンのみです"),
  email: z.string().email("有効なメールアドレスを入力してください"),
  password: z.string().min(1, "パスワードを入力してください"),
});

type FieldErrors = Partial<Record<keyof z.infer<typeof LoginSchema>, string>>;

interface ActionData {
  fieldErrors?: FieldErrors;
  formError?: string;
}

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

export const meta: MetaFunction = () => [{ title: "ログイン | HR SaaS" }];

// ---------------------------------------------------------------------------
// Action
// ---------------------------------------------------------------------------

export async function action({ request }: ActionFunctionArgs) {
  const formData = await request.formData();

  const raw = {
    slug: formData.get("slug"),
    email: formData.get("email"),
    password: formData.get("password"),
  };

  const result = LoginSchema.safeParse(raw);

  if (!result.success) {
    const fieldErrors: FieldErrors = {};
    for (const issue of result.error.issues) {
      const key = issue.path[0] as keyof FieldErrors;
      if (!fieldErrors[key]) {
        fieldErrors[key] = issue.message;
      }
    }
    return data<ActionData>({ fieldErrors }, { status: 422 });
  }

  // Forward the incoming Cookie so Go API can read the CSRF cookie
  const incomingCookie = request.headers.get("cookie");
  const apiResult = await apiLogin(incomingCookie, result.data);

  if (!apiResult.ok) {
    return data<ActionData>(
      { formError: "メールアドレス、パスワード、またはテナントIDが正しくありません。" },
      { status: 401 },
    );
  }

  // Forward all Set-Cookie headers from Go API to the browser
  const responseHeaders = new Headers({ Location: "/" });
  for (const cookie of apiResult.setCookieHeaders) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  return redirect("/", { headers: responseHeaders });
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function LoginPage() {
  const actionData = useActionData<ActionData>();
  const navigation = useNavigation();
  const isSubmitting = navigation.state === "submitting";

  return (
    <div style={styles.page}>
      <div style={styles.card}>
        <h1 style={styles.heading}>HR SaaS ログイン</h1>

        {actionData?.formError && (
          <p role="alert" style={styles.formError}>
            {actionData.formError}
          </p>
        )}

        <Form method="post" noValidate>
          <div style={styles.field}>
            <label htmlFor="slug" style={styles.label}>
              テナントID
            </label>
            <input
              id="slug"
              name="slug"
              type="text"
              autoComplete="organization"
              autoCapitalize="none"
              required
              aria-describedby={
                actionData?.fieldErrors?.slug ? "slug-error" : undefined
              }
              style={styles.input}
            />
            {actionData?.fieldErrors?.slug && (
              <span id="slug-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.slug}
              </span>
            )}
          </div>

          <div style={styles.field}>
            <label htmlFor="email" style={styles.label}>
              メールアドレス
            </label>
            <input
              id="email"
              name="email"
              type="email"
              autoComplete="username"
              required
              aria-describedby={
                actionData?.fieldErrors?.email ? "email-error" : undefined
              }
              style={styles.input}
            />
            {actionData?.fieldErrors?.email && (
              <span id="email-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.email}
              </span>
            )}
          </div>

          <div style={styles.field}>
            <label htmlFor="password" style={styles.label}>
              パスワード
            </label>
            <input
              id="password"
              name="password"
              type="password"
              autoComplete="current-password"
              required
              aria-describedby={
                actionData?.fieldErrors?.password ? "password-error" : undefined
              }
              style={styles.input}
            />
            {actionData?.fieldErrors?.password && (
              <span id="password-error" role="alert" style={styles.fieldError}>
                {actionData.fieldErrors.password}
              </span>
            )}
          </div>

          <button
            type="submit"
            disabled={isSubmitting}
            style={{
              ...styles.button,
              opacity: isSubmitting ? 0.6 : 1,
              cursor: isSubmitting ? "not-allowed" : "pointer",
            }}
          >
            {isSubmitting ? "ログイン中..." : "ログイン"}
          </button>
        </Form>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Minimal inline styles (no external CSS dependency)
// ---------------------------------------------------------------------------

const styles = {
  page: {
    minHeight: "100vh",
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    backgroundColor: "#f5f5f5",
    fontFamily: "system-ui, -apple-system, sans-serif",
  },
  card: {
    backgroundColor: "#ffffff",
    borderRadius: "8px",
    boxShadow: "0 2px 12px rgba(0,0,0,0.1)",
    padding: "2.5rem",
    width: "100%",
    maxWidth: "420px",
  },
  heading: {
    margin: "0 0 1.5rem",
    fontSize: "1.5rem",
    fontWeight: 600,
    color: "#1a1a1a",
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
  input: {
    padding: "0.625rem 0.75rem",
    border: "1px solid #d1d5db",
    borderRadius: "6px",
    fontSize: "1rem",
    outline: "none",
    transition: "border-color 0.15s",
  },
  fieldError: {
    marginTop: "0.25rem",
    fontSize: "0.8rem",
    color: "#dc2626",
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
  button: {
    width: "100%",
    padding: "0.75rem",
    backgroundColor: "#2563eb",
    color: "#ffffff",
    border: "none",
    borderRadius: "6px",
    fontSize: "1rem",
    fontWeight: 600,
    marginTop: "0.5rem",
  },
} as const;
