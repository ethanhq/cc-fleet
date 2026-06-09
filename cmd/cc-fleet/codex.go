package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// codexCredentialRef picks the secret_ref for a new codex provider: the default
// (cli-ride-capable) credential if no existing codex already holds it, otherwise the
// provider's own name — mirroring the TUI so the first codex reuses ~/.codex in both.
func codexCredentialRef(name string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	for _, v := range cfg.Vendors {
		if v.EffectiveProtocol() == config.ProtocolCodexOAuth && codexproxy.IsDefaultCredentialRef(v.SecretRef) {
			return name, nil
		}
	}
	return codexproxy.SecretRef, nil
}

// newCodexCmd builds `cc-fleet codex {login,logout,status,add}` — cc-fleet's own
// OAuth login for reusing a ChatGPT subscription, kept on an independent token
// chain that never touches ~/.codex (so the codex CLI's own login is unaffected).
func newCodexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Reuse a ChatGPT subscription as a cc-fleet provider",
	}

	var (
		acceptRisk bool
		loginCred  string
		logoutCred string
		statusCred string
	)
	login := &cobra.Command{
		Use:           "login",
		Short:         "Authorize cc-fleet against your ChatGPT subscription (device code)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !acceptRisk {
				if err := confirmCodexRisk(cmd); err != nil {
					return err
				}
			}
			return codexproxy.Login(cmd.Context(), cmd.OutOrStdout(), loginCred)
		},
	}
	login.Flags().BoolVar(&acceptRisk, "accept-risk", false, "skip the account-risk confirmation prompt")
	login.Flags().StringVar(&loginCred, "credential", "", "credential ref to log in (a codex provider's secret_ref; default: the legacy single credential)")
	cmd.AddCommand(login)

	logout := &cobra.Command{
		Use:           "logout",
		Short:         "Remove a codex login and stop its daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := codexproxy.Logout(logoutCred); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "logged out")
			return nil
		},
	}
	logout.Flags().StringVar(&logoutCred, "credential", "", "credential ref to log out (default: the legacy single credential)")
	cmd.AddCommand(logout)

	status := &cobra.Command{
		Use:   "status",
		Short: "Show codex credential sources (the codex CLI login and cc-fleet's own)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st := codexproxy.StatusReport(statusCred)
			out := cmd.OutOrStdout()
			ride := "unavailable"
			if st.CLIRide {
				ride = "available"
			}
			own := "absent"
			if st.OwnLogin {
				own = "present"
			}
			fmt.Fprintf(out, "cli-ride:  %s\n", ride)
			fmt.Fprintf(out, "own-login: %s\n", own)
			if st.Active == "none" {
				fmt.Fprintf(out, "active:    none — log in with the codex CLI, or run: %s\n", codexproxy.LoginHint(statusCred))
			} else {
				fmt.Fprintf(out, "active:    %s (account %s)\n", st.Active, st.Account)
			}
			return nil
		},
	}
	status.Flags().StringVar(&statusCred, "credential", "", "credential ref to inspect (default: the legacy single credential)")
	cmd.AddCommand(status)

	var (
		name  string
		port  int
		model string
	)
	add := &cobra.Command{
		Use:           "add",
		Short:         "Register the codex provider (auto-scans ~/.codex/config.toml for the model)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			chosen, err := codexproxy.ChoosePort(port)
			if err != nil {
				return err
			}
			if model == "" {
				model = codexproxy.ScanDefaultModel("gpt-5.5")
			}
			base := fmt.Sprintf("http://127.0.0.1:%d/", chosen)
			// The first codex claims the default (cli-ride-capable) credential so it can
			// reuse ~/.codex; a later one gets its own login keyed on its name. secret_ref
			// carries that credential id.
			ref, err := codexCredentialRef(name)
			if err != nil {
				return err
			}
			res, err := userops.Add(userops.AddRequest{
				Name:           name,
				BaseURL:        base,
				ModelsEndpoint: base + "v1/models",
				DefaultModel:   model,
				SecretBackend:  codexproxy.SecretBackend,
				SecretRef:      ref,
				Enabled:        true,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added provider %s (port %d, model %s)\n", res.Vendor, chosen, model)
			if loggedIn, _ := codexproxy.LoginStatus(ref); !loggedIn {
				if codexproxy.IsDefaultCredentialRef(ref) {
					fmt.Fprintln(cmd.OutOrStdout(), "next: cc-fleet codex login")
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "next: cc-fleet codex login --credential %s\n", ref)
				}
			}
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "codex", "provider name to register")
	add.Flags().IntVar(&port, "port", 0, "loopback port for the conversion daemon (default: first free in the reserved range)")
	add.Flags().StringVar(&model, "model", "", "default model (default: ~/.codex/config.toml, else gpt-5.5)")
	cmd.AddCommand(add)

	return cmd
}

// confirmCodexRisk prints the risk notice and asks for explicit confirmation.
// Non-interactive callers must pass --accept-risk (no silent opt-in).
func confirmCodexRisk(cmd *cobra.Command) error {
	fmt.Fprintln(cmd.OutOrStdout(), codexproxy.RiskNotice)
	if !term.IsTerminal(os.Stdin.Fd()) {
		return errors.New("non-interactive session: pass --accept-risk to confirm")
	}
	fmt.Fprint(cmd.OutOrStdout(), "Continue? [y/N] ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		return errors.New("aborted")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("aborted")
	}
}
