package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/OmniLLM/omni-agent-hub/internal/config"
)

func newConfigCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configuration utilities",
	}
	cmd.AddCommand(newConfigMigrateCmd(opts))
	cmd.AddCommand(newConfigShowCmd(opts))
	cmd.AddCommand(newConfigInitCmd(opts))
	return cmd
}

func newConfigMigrateCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Rewrite config.yaml in the new hub shape (in place)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := opts.ConfigPath
			if path == "" {
				path = config.DefaultConfigPath()
			}
			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("load %s: %w", path, err)
			}
			if err := config.Save(cfg, path); err != nil {
				return fmt.Errorf("save %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s Migrated %s to the new hub shape.\n", okGlyph(), bold(path))
			return nil
		},
	}
}

func newConfigShowCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the resolved config with defaults applied",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.LoadOrDefault(opts.ConfigPath)
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			sec := newKV("Configuration")
			sec.add("server.host", cfg.Server.Host)
			sec.add("server.port", cfg.Server.Port)
			sec.add("server.public_url", cfg.Server.PublicURL)
			sec.add("hub.name", cfg.Hub.Name)
			sec.add("storage.path", cfg.Storage.Path)
			sec.add("logging.file", cfg.Logging.File)
			sec.add("upstreams", len(cfg.Upstream))
			sec.flush(out)
			fmt.Fprintln(out)
			return nil
		},
	}
}
