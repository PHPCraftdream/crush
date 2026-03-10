import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { pluginBabel } from "@rsbuild/plugin-babel";
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
  plugins: [
    pluginReact(),
    pluginBabel({
      babelLoaderOptions(opts) {
        opts.plugins ??= [];
        // React Compiler must run before JSX is lowered to createElement.
        // pluginBabel inserts babel-loader AFTER SWC in the same rule, so
        // right-to-left execution means Babel runs first on raw source.
        opts.plugins.unshift("babel-plugin-react-compiler");
      },
    }),
  ],
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
