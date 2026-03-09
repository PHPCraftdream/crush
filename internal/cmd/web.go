package cmd

import "github.com/spf13/cobra"

func init() {
	webCmd.Flags().StringP("host", "H", "localhost", "Host to listen on")
	webCmd.Flags().IntP("port", "p", 0, "Port to listen on (0 = random free port)")
	webCmd.Flags().Bool("no-open", false, "Do not open browser automatically")
	rootCmd.AddCommand(webCmd)
}

// webCmd keeps `crush web` as an alias for `crush --web` for convenience.
var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Start crush in web mode (alias for crush --web)",
	Long: `Start crush in web mode.

Launches an HTTP server with a React-based web interface and a WebSocket
endpoint for real-time communication. All agent, session, and permission
interactions happen over WebSocket — no TUI is started.

A one-time access token is printed at startup; enter it in the browser to
authenticate. The token is never transmitted in the URL.

  crush web
  crush web --port 8080 --no-open
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebMode(cmd)
	},
}
