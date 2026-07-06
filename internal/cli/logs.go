package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/tail"
)

func newLogsCmd(opts *Opts) *cobra.Command {
	var (
		follow bool
		lines  int
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show and follow the server log file",
		Long:  "Display the last N lines of the omni-agent-hub server log and optionally follow new output in real time.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.LoadOrDefault(opts.ConfigPath)
			logPath := config.ResolveLogFile(opts.LogFile, cfg)

			// Print the last N lines.
			lastLines, err := tail.LastLines(logPath, lines)
			if err != nil {
				return fmt.Errorf("reading log file %s: %w", logPath, err)
			}
			out := cmd.OutOrStdout()
			for _, line := range lastLines {
				fmt.Fprintln(out, line)
			}

			if !follow {
				return nil
			}

			// Follow new output until interrupted.
			return tail.Follow(cmd.Context(), logPath, out)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "follow log output in real time")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of lines to show initially")

	return cmd
}
