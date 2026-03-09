/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        // Canvas — main page/card background (white in light, very dark in dark)
        canvas: "var(--color-canvas)",
        // Backgrounds
        base: {
          subtle:  "var(--color-base-subtle)",
          overlay: "var(--color-base-overlay)",
        },
        surface: "var(--color-surface)",
        text: {
          DEFAULT: "var(--color-text)",
          muted:   "var(--color-text-muted)",
          subtle:  "var(--color-text-subtle)",
        },
        accent:        "var(--color-accent)",
        "accent-fill": "var(--color-accent-fill)",
        mauve:         "#7c3aed",
        green:         "var(--color-green)",
        red:           "var(--color-red)",
        "red-fill":    "var(--color-red-fill)",
        yellow:        "var(--color-yellow)",
        "yellow-fill": "var(--color-yellow-fill)",
        blue:    "#0284c7",
        teal:    "#0d9488",
      },
      fontFamily: {
        mono: [
          "Cascadia Code",
          "Fira Code",
          "JetBrains Mono",
          "ui-monospace",
          "monospace",
        ],
      },
      boxShadow: {
        sm:  "0 1px 2px 0 rgb(0 0 0 / 0.05)",
        DEFAULT: "0 1px 3px 0 rgb(0 0 0 / 0.08), 0 1px 2px -1px rgb(0 0 0 / 0.08)",
        md:  "0 4px 6px -1px rgb(0 0 0 / 0.10), 0 2px 4px -2px rgb(0 0 0 / 0.10)",
        lg:  "0 10px 15px -3px rgb(0 0 0 / 0.15), 0 4px 6px -4px rgb(0 0 0 / 0.10)",
        xl:  "0 20px 25px -5px rgb(0 0 0 / 0.18), 0 8px 10px -6px rgb(0 0 0 / 0.12)",
      },
    },
  },
};
