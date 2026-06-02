import { redirect } from "react-router";
import type { ActionFunctionArgs } from "react-router";
import { apiLogout } from "~/lib/api.server";

/**
 * Logout route — action only.
 *
 * POST /logout:
 * 1. Calls Go API /auth/logout (with Cookie + CSRF token).
 * 2. Forwards any Set-Cookie headers from Go API (session deletion).
 * 3. Redirects to /login.
 *
 * GET requests are redirected to /login without calling the API,
 * so users who navigate here directly are handled gracefully.
 */
export async function action({ request }: ActionFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");
  const apiResult = await apiLogout(incomingCookie);

  const responseHeaders = new Headers({ Location: "/login" });

  // Forward session-clearing cookies from Go API
  for (const cookie of apiResult.setCookieHeaders) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  return redirect("/login", { headers: responseHeaders });
}

export async function loader() {
  // Direct navigation to /logout — just redirect to login
  return redirect("/login");
}
