import type { Config } from "@react-router/dev/config";

export default {
  // Server-side rendering enabled (Node/BFF mode)
  ssr: true,
  future: {
    v8_middleware: true,
    v8_splitRouteModules: true,
    v8_viteEnvironmentApi: true,
    v8_passThroughRequests: true,
    v8_trailingSlashAwareDataRequests: true,
  },
} satisfies Config;
