/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        // Canvas — main page/card background
        canvas:        "rgb(var(--color-canvas) / <alpha-value>)",
        // Backgrounds
        base: {
          subtle:  "rgb(var(--color-base-subtle) / <alpha-value>)",
          overlay: "rgb(var(--color-base-overlay) / <alpha-value>)",
        },
        surface:       "rgb(var(--color-surface) / <alpha-value>)",
        text: {
          DEFAULT: "rgb(var(--color-text) / <alpha-value>)",
          muted:   "rgb(var(--color-text-muted) / <alpha-value>)",
          subtle:  "rgb(var(--color-text-subtle) / <alpha-value>)",
        },
        accent:        "rgb(var(--color-accent) / <alpha-value>)",
        "accent-fill": "rgb(var(--color-accent-fill) / <alpha-value>)",
        mauve:         "#7c3aed",
        green:         "rgb(var(--color-green) / <alpha-value>)",
        red:           "rgb(var(--color-red) / <alpha-value>)",
        "red-fill":    "rgb(var(--color-red-fill) / <alpha-value>)",
        yellow:        "rgb(var(--color-yellow) / <alpha-value>)",
        "yellow-fill": "rgb(var(--color-yellow-fill) / <alpha-value>)",
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
