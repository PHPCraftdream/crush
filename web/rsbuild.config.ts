import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { execSync } from "child_process";

function git(cmd: string, fallback: string): string {
  try {
    return execSync(cmd, { encoding: "utf8" }).trim();
  } catch {
    return fallback;
  }
}

const GIT_COMMIT  = git("git rev-parse --short=7 HEAD", "unknown");
const GIT_COUNT   = git("git rev-list --count HEAD", "0");
const GIT_BRANCH  = git("git rev-parse --abbrev-ref HEAD", "unknown");

export default defineConfig({
  plugins: [pluginReact()],
  tools: {
    babel(config) {
      config.plugins ??= [];
      config.plugins.push("babel-plugin-react-compiler");
    },
  },
  source: {
    entry: { index: "./src/main.tsx" },
    define: {
      __GIT_COMMIT__: JSON.stringify(GIT_COMMIT),
      __GIT_COUNT__:  JSON.stringify(GIT_COUNT),
      __GIT_BRANCH__: JSON.stringify(GIT_BRANCH),
    },
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
