import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import yaml from "js-yaml";
import fs from "fs";
import path from "path";

// Dev-only: read the backend listen port from ../config.yaml to wire the vite
// dev-server proxy. In production builds (e.g. inside Docker) config.yaml is
// absent (and is a secret, excluded from the build context), so fall back to
// defaults — the proxy is only used by `vite dev`, never by `vite build`.
let backendPort = "8080";
let frontendPort = 5173;
try {
  const cfgPath = path.resolve(__dirname, "../config.yaml");
  if (fs.existsSync(cfgPath)) {
    const config = yaml.load(fs.readFileSync(cfgPath, "utf-8")) as {
      listen: string;
      frontend_port?: number;
    };
    backendPort = config.listen.replace(/^:/, "");
    frontendPort = config.frontend_port ?? 5173;
  }
} catch {
  // config.yaml unreadable during build — keep defaults
}
const backend = `http://localhost:${backendPort}`;

export default defineConfig({
  plugins: [react()],
  server: {
    port: frontendPort,
    host: '0.0.0.0',
    proxy: {
      "/admin": backend,
      "/v1": backend,
    },
  },
});
