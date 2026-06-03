import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  // Dashboard (root, auth-required)
  index("routes/dashboard.tsx"),

  // Auth
  route("login", "routes/login.tsx"),
  route("logout", "routes/logout.tsx"),

  // Attendance (#4 #23)
  route("attendance", "routes/attendance.tsx"),
  route("attendance/clock-in", "routes/attendance.clock-in.tsx"),

  // Self-service change requests (#4 #23)
  route("selfservice/change-requests", "routes/selfservice.change-requests.tsx"),
  route(
    "selfservice/change-requests/new",
    "routes/selfservice.change-requests.new.tsx",
  ),
] satisfies RouteConfig;
