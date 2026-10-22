import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Em desenvolvimento a API roda em :8080 (mspvdc api ou devstack).
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/v1": { target: "http://localhost:8080", ws: true },
      "/auth": "http://localhost:8080",
      "/config.json": "http://localhost:8080",
    },
  },
  build: { sourcemap: false, chunkSizeWarningLimit: 900 },
});
