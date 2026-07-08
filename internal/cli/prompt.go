package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// prompt helpers — pure stdlib, no external TUI dependencies.

var stdinReader = bufio.NewReader(os.Stdin)

// readLine prints a prompt and reads a trimmed line from stdin.
// Returns empty string on EOF.
func readLine(prompt string) string {
	fmt.Print(prompt)
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}

// readLineDefault prints a prompt with a default value hint.
// If the user presses Enter without typing, the default is returned.
func readLineDefault(prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// readLineMasked reads a line like readLineDefault but hints that the value
// is sensitive (shows first 8 chars of default, rest masked).
func readLineMasked(prompt, def string) string {
	hint := ""
	if def != "" {
		if len(def) > 8 {
			hint = def[:8] + "…"
		} else {
			hint = def
		}
	}
	if hint != "" {
		fmt.Printf("%s [%s]: ", prompt, hint)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// confirm prints a yes/no prompt. Default is the value returned on empty input.
func confirm(prompt string, def bool) bool {
	suffix := " [y/N]: "
	if def {
		suffix = " [Y/n]: "
	}
	fmt.Print(prompt + suffix)
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// readChoice presents numbered options and returns the selected index (0-based).
// Returns -1 if the user cancels (empty input or invalid).
func readChoice(prompt string, options []string) int {
	fmt.Println(bold(prompt))
	for i, opt := range options {
		fmt.Printf("  %s %s\n", cyan(fmt.Sprintf("%d)", i+1)), opt)
	}
	fmt.Print(dim("Choice: "))
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return -1
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return -1
	}
	return n - 1
}

// upstreamEntry is the subset of upstream info needed for interactive selection.
type upstreamEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Prefix  string `json:"prefix"`
	Status  string `json:"status"`
	Skills  int    `json:"skills"`
}

// selectUpstream fetches upstreams from the admin API and lets the user pick one.
// Returns nil if no upstreams exist or the user cancels.
func selectUpstream(c *adminClient) (*upstreamEntry, error) {
	ups, err := fetchUpstreams(c)
	if err != nil {
		return nil, err
	}
	if len(ups) == 0 {
		fmt.Println("No upstreams registered.")
		return nil, nil
	}

	fmt.Println("\n" + bold("Registered upstreams"))
	for i, u := range ups {
		fmt.Printf("  %s %s  %s\n",
			cyan(fmt.Sprintf("%d)", i+1)),
			padCell(u.Name, 20),
			dim(fmt.Sprintf("%s · %s · %d skills", u.BaseURL, u.Status, u.Skills)))
	}
	fmt.Print("\n" + dim("Select upstream [1]: "))
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		line = "1"
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(ups) {
		return nil, fmt.Errorf("invalid selection")
	}
	return &ups[n-1], nil
}

// fetchUpstreams calls GET /admin/upstreams and returns the list.
func fetchUpstreams(c *adminClient) ([]upstreamEntry, error) {
	resp, err := c.do("GET", "/admin/upstreams", nil)
	if err != nil {
		return nil, fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	var out []upstreamEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
