package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/doctor"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// initJSONEnvelope is the JSON shape `cc-fleet init --json` emits on success.
// We surface the underlying directory/file paths so the skill (and a future
// install.sh wrapper) can verify the tree without re-running stat itself,
// plus the inline doctor result so the user gets one-shot reassurance.
type initJSONEnvelope struct {
	OK         bool                 `json:"ok"`
	Created    []string             `json:"created"`
	AlreadyHad []string             `json:"already_had"`
	Doctor     *doctor.DoctorResult `json:"doctor,omitempty"`
}

func newInitCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the cc-fleet config tree and (optionally) add a first vendor",
		Long: `Create the cc-fleet config directory hierarchy:

  ~/.config/cc-fleet/          (mode 0700)
  ~/.config/cc-fleet/secrets/  (mode 0700)
  ~/.config/cc-fleet/vendors.toml (empty, mode 0600, schema version 1)
  ~/.claude/profiles/          (mode 0700)
  ~/.claude/skills/cc-fleet/  (mode 0700; contents installed separately)

In interactive mode (no --json), init asks whether to add a first vendor
right away. Replying "y" drops you into the same prompts that ` + "`cc-fleet add`" + `
would. Anything else (including bare Enter / EOF) skips the vendor step.

In --json mode, init is non-interactive: it creates the tree and runs the
nine doctor checks once, then prints a single JSON envelope.

Init is idempotent — running on a HOME that's already initialized just
reports each existing path under "already_had" and does not overwrite
vendors.toml.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runInit(asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Skip interactive prompts and emit a JSON envelope on stdout (for skill consumption)")

	return cmd
}

func runInit(asJSON bool) error {
	res, err := userops.Init()
	if err != nil {
		reportUserOpErr(asJSON, err)
		return err
	}

	// Always run doctor at the end — gives users one-shot reassurance after
	// init and a verifiable JSON checkpoint for automation.
	dres := doctor.RunAll(false)

	if asJSON {
		emitJSON(initJSONEnvelope{
			OK:         true,
			Created:    res.Created,
			AlreadyHad: res.AlreadyHad,
			Doctor:     &dres,
		})
		return nil
	}

	// Pretty mode.
	fmt.Println("Initialize cc-fleet... done.")
	if cfgDir := firstWithSuffix(res.Created, "cc-fleet"); cfgDir != "" {
		fmt.Println("Configuration:", cfgDir)
	} else if cfgDir := firstWithSuffix(res.AlreadyHad, "cc-fleet"); cfgDir != "" {
		fmt.Println("Configuration:", cfgDir)
	}
	fmt.Println()

	// Doctor block — same grouped output `cc-fleet doctor` prints, no fix attempts.
	fmt.Println("Health checks:")
	printDoctorGroup("Core", doctor.GroupCore, dres.Results)
	printDoctorGroup("Optional — live teammates only", doctor.GroupOptional, dres.Results)
	if dres.OK {
		fmt.Println("core checks passed")
	} else {
		fmt.Println("one or more core checks failed; see hints above (run: cc-fleet doctor --fix)")
	}
	fmt.Println()

	// Interactive vendor-add prompt. Reads one line; only "y"/"yes" branches
	// into the add flow. Everything else (Enter, EOF, "n") skips.
	reader := bufio.NewReader(os.Stdin)
	ans, _ := promptLine(reader, "Add a vendor now? (y/N): ")
	ans = strings.TrimSpace(strings.ToLower(ans))
	if ans != "y" && ans != "yes" {
		return nil
	}

	if !isTTY(os.Stdin) {
		fmt.Fprintln(os.Stderr, "init: cannot run interactive add without a tty; rerun `cc-fleet add <vendor>` with flags")
		return nil
	}

	if err := interactiveAdd(reader); err != nil {
		// Surface the error but don't fail init itself — the user can re-try
		// add later.
		fmt.Fprintf(os.Stderr, "init: vendor add aborted: %s\n", err)
	}
	return nil
}

// firstWithSuffix returns the first entry in paths whose basename equals
// suffix, or "" if none match. Used to surface "Configuration: <dir>" from
// either Created or AlreadyHad.
func firstWithSuffix(paths []string, suffix string) string {
	for _, p := range paths {
		// We want the cc-fleet config dir specifically (not vendors.toml).
		// "cc-fleet" is the last path element for that case.
		if strings.HasSuffix(p, "/"+suffix) || p == suffix {
			return p
		}
	}
	return ""
}

// interactiveAdd walks the user through the same fields `cc-fleet add` would
// require on the command line. It's intentionally tiny — just enough to
// bootstrap a first vendor without leaving the init flow.
func interactiveAdd(reader *bufio.Reader) error {
	name, err := promptLine(reader, "  vendor name (e.g. glm, deepseek): ")
	if err != nil {
		return err
	}
	if err := userops.ValidateVendorName(name); err != nil {
		return err
	}
	baseURL, err := promptLine(reader, "  base_url: ")
	if err != nil {
		return err
	}
	modelsEP, err := promptLine(reader, "  models_endpoint: ")
	if err != nil {
		return err
	}
	defModel, err := promptLine(reader, "  default_model: ")
	if err != nil {
		return err
	}
	// Read the API key WITHOUT echo (promptPassword uses term.ReadPassword) so the
	// plaintext key never lands in the terminal or scrollback. interactiveAdd only
	// runs after runInit's isTTY check; promptPassword also carries its own non-TTY
	// guard pointing at --api-key-stdin.
	apiKey, err := promptPassword("  API key (stored under ~/.config/cc-fleet/secrets/): ")
	if err != nil {
		return err
	}

	res, addErr := userops.Add(userops.AddRequest{
		Name:           name,
		BaseURL:        baseURL,
		ModelsEndpoint: modelsEP,
		DefaultModel:   defModel,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		APIKey:         apiKey,
		Enabled:        true,
	})
	if addErr != nil {
		return addErr
	}
	fmt.Printf("  added %s (profile: %s, models: %d)\n",
		res.Vendor, res.ProfilePath, res.ModelCount)
	return nil
}
