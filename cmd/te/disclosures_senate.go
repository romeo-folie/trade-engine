package main

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/disclosures"
	"github.com/spf13/cobra"
)

var disclosureSenateAuthorizeTimeout time.Duration

var disclosureSenateCmd = &cobra.Command{
	Use:   "senate",
	Short: "Manage the persistent Senate eFD browser authorization",
}

var disclosureSenateAuthorizeCmd = &cobra.Command{
	Use:   "authorize",
	Short: "Open Chrome once and authorize the dedicated Senate eFD profile",
	Long: `Opens a visible Chrome window backed by TradeEngine's dedicated Senate
profile. Review and accept the official eFD agreement in that window. The
command returns once the profile can access the disclosure search page.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		browser, err := configuredSenateBrowser(false)
		if err != nil {
			return err
		}
		defer browser.Close() //nolint:errcheck
		ctx, cancel := context.WithTimeout(cmd.Context(), disclosureSenateAuthorizeTimeout)
		defer cancel()
		fmt.Printf("Opening Senate eFD in Chrome. Review and accept the agreement there.\nProfile: %s\n",
			browser.ProfileDir())
		if err := browser.Authorize(ctx); err != nil {
			return err
		}
		fmt.Println("Senate eFD browser profile is authorized and ready for automatic sync.")
		return nil
	},
}

var disclosureSenateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check the persistent Senate eFD browser authorization",
	RunE: func(cmd *cobra.Command, _ []string) error {
		browser, err := configuredSenateBrowser(true)
		if err != nil {
			return err
		}
		defer browser.Close() //nolint:errcheck
		authorized, err := browser.AuthorizationStatus(cmd.Context())
		if err != nil {
			return err
		}
		if !authorized {
			return fmt.Errorf("%w (profile: %s)",
				disclosures.ErrSenateAuthorizationRequired, browser.ProfileDir())
		}
		fmt.Printf("Senate eFD browser profile is authorized.\nProfile: %s\n", browser.ProfileDir())
		return nil
	},
}

var disclosureSenateLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Delete the dedicated Senate eFD Chrome profile",
	RunE: func(_ *cobra.Command, _ []string) error {
		browser, err := configuredSenateBrowser(true)
		if err != nil {
			return err
		}
		profileDir := browser.ProfileDir()
		if err := disclosures.RemoveSenateBrowserProfile(profileDir); err != nil {
			return err
		}
		fmt.Printf("Removed Senate eFD browser profile: %s\n", profileDir)
		return nil
	},
}

func configuredSenateBrowser(background bool) (*disclosures.SenateChromeTransport, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.Disclosures.RequestTimeoutSeconds) * time.Second
	return disclosures.NewSenateChromeTransport(disclosures.SenateBrowserOptions{
		BaseURL:        cfg.Disclosures.Senate.BaseURL,
		ProfileDir:     cfg.Disclosures.Senate.BrowserProfileDir,
		ExecutablePath: cfg.Disclosures.Senate.BrowserExecutable,
		Background:     background,
		Timeout:        timeout,
	})
}

func init() {
	disclosureSenateAuthorizeCmd.Flags().DurationVar(
		&disclosureSenateAuthorizeTimeout, "timeout", 10*time.Minute,
		"Maximum time to wait for agreement acceptance",
	)
	disclosureSenateCmd.AddCommand(
		disclosureSenateAuthorizeCmd,
		disclosureSenateStatusCmd,
		disclosureSenateLogoutCmd,
	)
}
