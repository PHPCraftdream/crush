package cmd

import "github.com/spf13/cobra"

func init() {
	webCmd.Flags().StringP("host", "H", "localhost", "Host to listen on")
	webCmd.Flags().IntP("port", "p", 0, "Port to listen on (0 = random free port)")
	webCmd.Flags().Bool("no-open", false, "Do not open browser automatically")
	rootCmd.AddCommand(webCmd)
}

// webCmd is an explicit alias for the bare ` + "`crush`" + ` command. Both start the
// embedded web server — webCmd just makes the intent obvious in scripts.
var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Start the Crush web UI (same as running 'crush' with no subcommand)",
	Long: `Start the Crush web UI.

Launches an HTTP server with a React UI and a WebSocket endpoint for
real-time communication with the agent. Sessions, permissions, model
selection, logs, message queueing, and turn-interruption (the yellow
"Interrupt" button that cancels the running turn while keeping
everything produced so far) all happen in the browser.

At startup the server prints the URL and a one-time access token. The
token is also copied to your clipboard. Paste it in the browser to
authenticate — it is never transmitted as part of the URL.

  crush web
  crush web --port 8080 --no-open
  crush web --host 0.0.0.0 --port 9000   # bind on all interfaces
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebMode(cmd)
	},
}
