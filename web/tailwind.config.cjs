/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Backgrounds — no DEFAULT to avoid conflict with text-base font-size utility
        base: {
          subtle:  "#f8fafc",   // sidebar, panels
          overlay: "#f1f5f9",   // inputs, cards
        },
        surface: "#e2e8f0",     // borders, dividers
        text: {
          DEFAULT: "#0f172a",   // near-black
          muted:   "#475569",   // secondary
          subtle:  "#94a3b8",   // placeholder / hints
        },
        accent:  "#4f46e5",     // indigo
        mauve:   "#7c3aed",     // purple
        green:   "#16a34a",
        red:     "#dc2626",
        yellow:  "#d97706",
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
        md:  "0 4px 6px -1px rgb(0 0 0 / 0.08), 0 2px 4px -2px rgb(0 0 0 / 0.08)",
        lg:  "0 10px 15px -3px rgb(0 0 0 / 0.08), 0 4px 6px -4px rgb(0 0 0 / 0.08)",
        xl:  "0 20px 25px -5px rgb(0 0 0 / 0.08), 0 8px 10px -6px rgb(0 0 0 / 0.08)",
      },
    },
  },
};
