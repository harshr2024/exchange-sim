import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The Go server has no CORS headers. The dev server proxies HTTP and the
// WebSocket so the browser talks to one origin and the exchange stays on :8080.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", changeOrigin: true },
      "/healthz": { target: "http://127.0.0.1:8080", changeOrigin: true },
      "/ws": { target: "http://127.0.0.1:8080", ws: true, changeOrigin: true },
    },
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
