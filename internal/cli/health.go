package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newHealthCmd(opts *Opts) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Show upstream health overview",
		Long:  "Display a dashboard of all upstream agents and their current health status.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newAdminClient(opts)
			resp, err := c.do("GET", "/admin/health", nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var data struct {
				Upstreams []struct {
					ID                  string `json:"id"`
					Name                string `json:"name"`
					BaseURL             string `json:"base_url"`
					Prefix              string `json:"prefix"`
					Enabled             bool   `json:"enabled"`
					Status              string `json:"status"`
					ConsecutiveFailures int    `json:"consecutive_failures"`
					LastSuccessAt       string `json:"last_success_at"`
					LastFailureAt       string `json:"last_failure_at"`
					SkillCount          int    `json:"skill_count"`
				} `json:"upstreams"`
				Summary struct {
					Total     int `json:"total"`
					Healthy   int `json:"healthy"`
					Unhealthy int `json:"unhealthy"`
					Unknown   int `json:"unknown"`
					Enabled   int `json:"enabled"`
				} `json:"summary"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(data)
			}

			// Summary header.
			fmt.Fprintln(out)
			fmt.Fprintln(out, summaryLine("Upstreams",
				summaryPart{"total", data.Summary.Total},
				summaryPart{green("healthy"), data.Summary.Healthy},
				summaryPart{red("unhealthy"), data.Summary.Unhealthy},
				summaryPart{yellow("unknown"), data.Summary.Unknown},
			))
			fmt.Fprintln(out)

			if len(data.Upstreams) == 0 {
				fmt.Fprintln(out, dim("  No upstreams registered."))
				return nil
			}

			// Table.
			tbl := newTable("NAME", "STATUS", "FAILS", "SKILLS", "LAST SUCCESS", "LAST FAILURE", "URL")
			tbl.alignRight(2, 3)
			for _, u := range data.Upstreams {
				tbl.row(
					u.Name,
					statusDot(u.Status),
					u.ConsecutiveFailures,
					u.SkillCount,
					formatTimeSince(u.LastSuccessAt),
					formatTimeSince(u.LastFailureAt),
					u.BaseURL,
				)
			}
			tbl.flush(out)
			fmt.Fprintln(out)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}
