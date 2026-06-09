package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
)

// newCodexProxyCmd builds `cc-fleet codex-proxy {serve,stop,status}` — the local
// Anthropic<->OpenAI conversion daemon. `serve` is started detached by the run
// modes; users normally only touch `stop`/`status`.
func newCodexProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex-proxy",
		Short: "Manage the codex conversion daemon",
	}
	var (
		port        int
		protocol    string
		upstreamURL string
		credential  string
	)
	serve := &cobra.Command{
		Use:           "serve",
		Short:         "Run the conversion daemon (loopback only)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if port <= 0 {
				return fmt.Errorf("--port is required")
			}
			return codexproxy.Serve(port, protocol, upstreamURL, credential)
		},
	}
	serve.Flags().IntVar(&port, "port", 0, "loopback port to bind")
	serve.Flags().StringVar(&protocol, "protocol", "", "wire protocol (default: codex-oauth)")
	serve.Flags().StringVar(&upstreamURL, "upstream-url", "", "real upstream base URL (openai-* protocols)")
	serve.Flags().StringVar(&credential, "credential", "", "codex credential ref (default: the legacy single credential)")
	cmd.AddCommand(serve)
	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the codex conversion daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return codexproxy.StopDaemon()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the conversion daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			daemons := codexproxy.RunningDaemons()
			if len(daemons) == 0 {
				fmt.Println("not running")
				return nil
			}
			for _, d := range daemons {
				fmt.Printf("running on 127.0.0.1:%d (%s)\n", d.Port, d.Protocol)
			}
			return nil
		},
	})
	return cmd
}
