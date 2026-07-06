package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/OmniLLM/omni-agent-hub/internal/config"
)

func newUpstreamCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "upstream",
		Aliases: []string{"up"},
		Short:   "Manage upstream A2A agents (via admin API)",
	}
	cmd.AddCommand(newUpstreamListCmd(opts))
	cmd.AddCommand(newUpstreamAddCmd(opts))
	cmd.AddCommand(newUpstreamRemoveCmd(opts))
	cmd.AddCommand(newUpstreamRefreshCmd(opts))
	cmd.AddCommand(newUpstreamEditCmd(opts))
	return cmd
}

// adminClient talks to the local hub over its admin API.
type adminClient struct {
	baseURL  string
	adminKey string
	http     *http.Client
}

func newAdminClient(opts *Opts) *adminClient {
	cfg := config.LoadOrDefault(opts.ConfigPath)
	host := cfg.Server.Host
	if host == "" || host == "0.0.0.0" {
		host = "localhost"
	}
	return &adminClient{
		baseURL:  fmt.Sprintf("http://%s:%d", host, cfg.Server.Port),
		adminKey: cfg.Server.AdminKey,
		http:     &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *adminClient) do(method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.adminKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.adminKey)
	}
	return c.http.Do(req)
}

func newUpstreamListCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered upstream agents",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c := newAdminClient(opts)
			resp, err := c.do("GET", "/admin/upstreams", nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return httpErr(resp)
			}
			var out []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				BaseURL string `json:"base_url"`
				Prefix  string `json:"prefix"`
				Status  string `json:"status"`
				HasCard bool   `json:"has_card"`
				Skills  int    `json:"skills"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return err
			}
			if len(out) == 0 {
				fmt.Println("No upstreams registered.")
				return nil
			}
			fmt.Printf("%-24s %-40s %-12s %-9s %s\n", "NAME", "BASE_URL", "PREFIX", "STATUS", "SKILLS")
			fmt.Println(strings.Repeat("─", 100))
			for _, u := range out {
				fmt.Printf("%-24s %-40s %-12s %-9s %d\n",
					u.Name, u.BaseURL, u.Prefix, u.Status, u.Skills)
			}
			return nil
		},
	}
}

func newUpstreamAddCmd(opts *Opts) *cobra.Command {
	var (
		url    string
		prefix string
		token  string
		scheme string
	)
	cmd := &cobra.Command{
		Use:   "add [name]",
		Short: "Register a new upstream agent",
		Long: `Register a new upstream agent.

When called with --url, operates non-interactively.
When called without flags, enters interactive mode and prompts for each field.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}

			// Interactive mode: no name arg or no --url flag.
			if name == "" || url == "" {
				fmt.Println("── Add upstream (interactive) ──")
				if name == "" {
					name = readLine("Name: ")
					if name == "" {
						return fmt.Errorf("name is required")
					}
				}
				if url == "" {
					url = readLine("Base URL: ")
					if url == "" {
						return fmt.Errorf("url is required")
					}
				}
				prefix = readLineDefault("Prefix", prefix)
				idx := readChoice("Auth scheme:", []string{"bearer", "none"})
				if idx >= 0 {
					scheme = []string{"bearer", "none"}[idx]
				}
				if scheme == "bearer" {
					token = readLine("Bearer token: ")
				}

				fmt.Println()
				fmt.Printf("  Name   : %s\n", name)
				fmt.Printf("  URL    : %s\n", url)
				fmt.Printf("  Prefix : %s\n", prefix)
				fmt.Printf("  Scheme : %s\n", scheme)
				if scheme == "bearer" && token != "" {
					fmt.Printf("  Token  : %s…\n", token[:min(8, len(token))])
				}
				fmt.Println()
				if !confirm("Register this upstream?", true) {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			payload := map[string]any{
				"name":     name,
				"base_url": url,
				"prefix":   prefix,
				"auth":     map[string]string{"scheme": scheme, "token": token},
			}
			body, _ := json.Marshal(payload)
			c := newAdminClient(opts)
			resp, err := c.do("POST", "/admin/upstreams", body)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				return httpErr(resp)
			}
			fmt.Printf("✓ Registered upstream '%s' (%s)\n", name, url)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "upstream base URL (required in non-interactive mode)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "optional routing prefix")
	cmd.Flags().StringVar(&token, "token", "", "bearer token for upstream")
	cmd.Flags().StringVar(&scheme, "scheme", "bearer", "auth scheme: bearer | none")
	return cmd
}

func newUpstreamRemoveCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:     "remove [id-or-name]",
		Aliases: []string{"rm"},
		Short:   "Unregister an upstream agent",
		Long: `Unregister an upstream agent by id or name.

When called without arguments, enters interactive mode: lists all registered
upstreams and lets you select one to remove.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := newAdminClient(opts)

			var target string
			var displayName string

			if len(args) > 0 {
				// Non-interactive: use the argument directly.
				target = args[0]
				displayName = target
			} else {
				// Interactive: let the user pick from the list.
				u, err := selectUpstream(c)
				if err != nil {
					return err
				}
				if u == nil {
					return nil // empty list
				}
				target = u.ID
				displayName = u.Name

				if !confirm(fmt.Sprintf("Remove upstream '%s' (%s)?", u.Name, u.BaseURL), false) {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			// Try by the given arg first; if 404 and it looks like a name, look up the ID.
			resp, err := c.do("DELETE", "/admin/upstreams/"+target, nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound && len(args) > 0 {
				// The arg might be a name, not a UUID — resolve it.
				ups, ferr := fetchUpstreams(c)
				if ferr != nil {
					return httpErr(resp) // fall back to original error
				}
				for _, u := range ups {
					if strings.EqualFold(u.Name, target) {
						resp2, err2 := c.do("DELETE", "/admin/upstreams/"+u.ID, nil)
						if err2 != nil {
							return err2
						}
						defer resp2.Body.Close()
						if resp2.StatusCode != http.StatusNoContent {
							return httpErr(resp2)
						}
						fmt.Printf("✓ Removed upstream '%s'\n", displayName)
						return nil
					}
				}
				return httpErr(resp) // name not found either
			}

			if resp.StatusCode != http.StatusNoContent {
				return httpErr(resp)
			}
			fmt.Printf("✓ Removed upstream '%s'\n", displayName)
			return nil
		},
	}
}

func newUpstreamRefreshCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Re-fetch all upstream agent cards",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c := newAdminClient(opts)
			resp, err := c.do("POST", "/admin/refresh", nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return httpErr(resp)
			}
			fmt.Println("✓ Upstream cards refreshed")
			return nil
		},
	}
}

func httpErr(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func newUpstreamEditCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:   "edit [name]",
		Short: "Interactively edit an upstream agent's configuration",
		Long: `Edit an existing upstream agent.

Shows current values and lets you change any field. Press Enter to keep
the current value. Internally removes and re-adds the upstream.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := newAdminClient(opts)

			// Resolve which upstream to edit.
			var target *upstreamEntry
			if len(args) > 0 {
				ups, err := fetchUpstreams(c)
				if err != nil {
					return err
				}
				for i := range ups {
					if strings.EqualFold(ups[i].Name, args[0]) || ups[i].ID == args[0] {
						target = &ups[i]
						break
					}
				}
				if target == nil {
					return fmt.Errorf("upstream '%s' not found", args[0])
				}
			} else {
				var err error
				target, err = selectUpstream(c)
				if err != nil {
					return err
				}
				if target == nil {
					return nil // empty list
				}
			}

			fmt.Printf("\n── Edit upstream '%s' ──\n", target.Name)
			fmt.Println("Press Enter to keep the current value.")

			newName := readLineDefault("Name", target.Name)
			newURL := readLineDefault("Base URL", target.BaseURL)
			newPrefix := readLineDefault("Prefix", target.Prefix)

			idx := readChoice("Auth scheme:", []string{"bearer", "none"})
			newScheme := "bearer"
			if idx >= 0 {
				newScheme = []string{"bearer", "none"}[idx]
			}
			newToken := ""
			if newScheme == "bearer" {
				newToken = readLineMasked("Bearer token", "")
			}

			fmt.Println()
			fmt.Printf("  Name   : %s\n", newName)
			fmt.Printf("  URL    : %s\n", newURL)
			fmt.Printf("  Prefix : %s\n", newPrefix)
			fmt.Printf("  Scheme : %s\n", newScheme)
			if newScheme == "bearer" && newToken != "" {
				fmt.Printf("  Token  : %s…\n", newToken[:min(8, len(newToken))])
			}
			fmt.Println()
			if !confirm("Apply changes?", true) {
				fmt.Println("Cancelled.")
				return nil
			}

			// Remove the old entry.
			resp, err := c.do("DELETE", "/admin/upstreams/"+target.ID, nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("failed to remove old upstream: %w", httpErr(resp))
			}

			// Re-add with updated values.
			payload := map[string]any{
				"name":     newName,
				"base_url": newURL,
				"prefix":   newPrefix,
				"auth":     map[string]string{"scheme": newScheme, "token": newToken},
			}
			body, _ := json.Marshal(payload)
			resp2, err := c.do("POST", "/admin/upstreams", body)
			if err != nil {
				return fmt.Errorf("removed old upstream but failed to re-add: %w", err)
			}
			defer resp2.Body.Close()
			if resp2.StatusCode != http.StatusCreated {
				return fmt.Errorf("removed old upstream but failed to re-add: %w", httpErr(resp2))
			}

			fmt.Printf("✓ Updated upstream '%s'\n", newName)
			return nil
		},
	}
}
