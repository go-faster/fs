import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { writeFileSync } from "node:fs";

// The built SPA (dist/) is committed so a plain `go build` embeds the real UI.
// emptyOutDir wipes the dist/.gitkeep placeholder on every build, so recreate it
// once the bundle is written to keep the go:embed directive happy on a clean
// checkout even before the first build.
function keepPlaceholder() {
  return {
    name: "keep-dist-placeholder",
    closeBundle() {
      writeFileSync("dist/.gitkeep", "");
    },
  };
}

// The built SPA is embedded into the Go binary (internal/adminhandler/ui.go)
// and served from the admin API's origin, so assets use relative paths.
export default defineConfig({
  plugins: [react(), keepPlaceholder()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5273,
    // During `npm run dev`, proxy API calls to a locally running fs admin.
    proxy: {
      "/api": "http://127.0.0.1:8090",
    },
  },
});
