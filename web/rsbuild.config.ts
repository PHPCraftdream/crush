import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";

export default defineConfig({
  plugins: [pluginReact()],
  source: {
    entry: { index: "./src/main.tsx" },
  },
  server: {
    port: 3000,
    proxy: {
      "/ws": {
        target: "ws://localhost:3030",
        ws: true,
      },
    },
  },
  output: {
    distPath: { root: "dist" },
    sourceMap: { js: "source-map", css: false },
  },
  html: {
    title: "Crush",
  },
});
