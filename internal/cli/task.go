package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

func newTaskCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "task",
		Aliases: []string{"tasks"},
		Short:   "Manage tasks (list, inspect, cancel)",
	}
	cmd.AddCommand(newTaskListCmd(opts))
	cmd.AddCommand(newTaskInspectCmd(opts))
	cmd.AddCommand(newTaskCancelCmd(opts))
	return cmd
}

func newTaskListCmd(opts *Opts) *cobra.Command {
	var (
		state     string
		upstream  string
		contextID string
		recent    bool
		limit     int
		offset    int
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks",
		Long:    "List active tasks. Use --recent to include completed/failed/canceled tasks.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newAdminClient(opts)

			path := fmt.Sprintf("/admin/tasks?limit=%d&offset=%d", limit, offset)
			if recent {
				path += "&recent=true"
			}
			if state != "" {
				path += "&state=" + state
			}
			if upstream != "" {
				path += "&upstream_id=" + upstream
			}
			if contextID != "" {
				path += "&context_id=" + contextID
			}

			resp, err := c.do("GET", path, nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var data struct {
				Items []struct {
					HubTaskID      string `json:"hub_task_id"`
					ContextID      string `json:"context_id"`
					UpstreamID     string `json:"upstream_id"`
					UpstreamTaskID string `json:"upstream_task_id"`
					State          string `json:"state"`
					CreatedAt      string `json:"created_at"`
					UpdatedAt      string `json:"updated_at"`
					HasSnapshot    bool   `json:"has_snapshot"`
				} `json:"items"`
				Total  int `json:"total"`
				Counts struct {
					Submitted     int `json:"submitted"`
					Working       int `json:"working"`
					InputRequired int `json:"input_required"`
					Completed     int `json:"completed"`
					Failed        int `json:"failed"`
					Canceled      int `json:"canceled"`
					Total         int `json:"total"`
				} `json:"counts"`
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

			// Summary.
			ct := data.Counts
			fmt.Fprintln(out)
			fmt.Fprintln(out, summaryLine("Tasks",
				summaryPart{cyan("submitted"), ct.Submitted},
				summaryPart{cyan("working"), ct.Working},
				summaryPart{yellow("input-req"), ct.InputRequired},
				summaryPart{green("completed"), ct.Completed},
				summaryPart{red("failed"), ct.Failed},
				summaryPart{dim("canceled"), ct.Canceled},
			))
			fmt.Fprintln(out)

			if len(data.Items) == 0 {
				fmt.Fprintln(out, dim("  No tasks found."))
				return nil
			}

			tbl := newTable("TASK_ID", "STATE", "UPSTREAM", "CONTEXT", "UPDATED", "UP_TASK")
			for _, t := range data.Items {
				tbl.row(
					shortID(t.HubTaskID),
					statusDot(t.State),
					shortID(t.UpstreamID),
					shortID(t.ContextID),
					formatTimeShort(t.UpdatedAt),
					shortID(t.UpstreamTaskID),
				)
			}
			tbl.flush(out)
			fmt.Fprintf(out, "\n%s\n\n", dim(fmt.Sprintf("Showing %d of %d", len(data.Items), data.Total)))
			return nil
		},
	}

	cmd.Flags().StringVar(&state, "state", "", "filter by state(s), comma-separated")
	cmd.Flags().StringVar(&upstream, "upstream", "", "filter by upstream ID")
	cmd.Flags().StringVar(&contextID, "context", "", "filter by context ID")
	cmd.Flags().BoolVar(&recent, "recent", false, "include terminal (completed/failed/canceled) tasks")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "pagination offset")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func newTaskInspectCmd(opts *Opts) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "inspect [task-id]",
		Short: "Show detailed task information",
		Long: `Show detailed information about a task.

If no task-id is provided, interactively select from active tasks.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newAdminClient(opts)

			taskID := ""
			if len(args) > 0 {
				taskID = args[0]
			} else {
				// Interactive: pick from active tasks.
				entry, err := selectTask(c, true)
				if err != nil {
					return err
				}
				if entry == nil {
					return nil
				}
				taskID = entry.HubTaskID
			}

			resolvedID, err := resolveTaskID(c, taskID)
			if err != nil {
				return err
			}
			taskID = resolvedID

			resp, err := c.do("GET", "/admin/tasks/"+taskID, nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var data map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(data)
			}

			fmt.Fprintln(out)
			sec := newKV("")
			sec.add("Hub Task ID", data["hub_task_id"])
			sec.add("Context ID", data["context_id"])
			sec.add("Upstream ID", data["upstream_id"])
			sec.add("Upstream Task ID", data["upstream_task_id"])
			sec.add("State", statusDot(fmt.Sprint(data["state"])))
			sec.add("Created", data["created_at"])
			sec.add("Updated", data["updated_at"])
			sec.flush(out)
			if task, ok := data["task"]; ok && task != nil {
				fmt.Fprintf(out, "\n  %s\n", bold("Task snapshot"))
				if err := printTaskSnapshot(out, task); err != nil {
					return err
				}
			}
			fmt.Fprintln(out)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func newTaskCancelCmd(opts *Opts) *cobra.Command {
	var (
		yes    bool
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "cancel [task-id]",
		Short: "Cancel an active task",
		Long: `Cancel an active task by forwarding a cancel request to the upstream.

If no task-id is provided, interactively select from active tasks.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newAdminClient(opts)

			taskID := ""
			displayID := ""
			if len(args) > 0 {
				taskID = args[0]
				displayID = shortID(taskID)
			} else {
				entry, err := selectTask(c, false)
				if err != nil {
					return err
				}
				if entry == nil {
					return nil
				}
				taskID = entry.HubTaskID
				displayID = shortID(taskID) + " (" + entry.State + ")"
			}

			if !yes && !confirm(fmt.Sprintf("Cancel task %s?", displayID), false) {
				fmt.Println(dim("Cancelled."))
				return nil
			}

			resolvedID, err := resolveTaskID(c, taskID)
			if err != nil {
				return err
			}
			taskID = resolvedID

			resp, err := c.do("POST", "/admin/tasks/"+taskID+"/cancel", nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()

			out := cmd.OutOrStdout()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			if asJSON {
				var data map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
					return err
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(data)
			}

			fmt.Fprintf(out, "%s Cancel requested for task %s\n", okGlyph(), bold(shortID(taskID)))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func printTaskSnapshot(out io.Writer, task any) error {
	b, err := json.Marshal(task)
	if err != nil {
		return err
	}
	var snap struct {
		TaskID    string `json:"id"`
		ContextID string `json:"contextId"`
		Status    struct {
			State   string `json:"state"`
			Message *struct {
				MessageID string `json:"messageId"`
				Role      string `json:"role"`
				Parts     []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"status"`
		Artifacts []struct {
			ArtifactID string `json:"artifactId"`
			Name       string `json:"name"`
			Parts      []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"artifacts"`
		History []struct {
			MessageID string `json:"messageId"`
			Role      string `json:"role"`
			Parts     []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"history"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return err
	}

	fmt.Fprintf(out, "    id            : %s\n", snap.TaskID)
	fmt.Fprintf(out, "    contextId     : %s\n", snap.ContextID)
	fmt.Fprintf(out, "    state         : %s\n", statusDot(snap.Status.State))
	if snap.Status.Message != nil {
		fmt.Fprintf(out, "    %s\n", dim("status message:"))
		fmt.Fprintf(out, "      role         : %s\n", snap.Status.Message.Role)
		fmt.Fprintf(out, "      messageId    : %s\n", snap.Status.Message.MessageID)
		for i, p := range snap.Status.Message.Parts {
			if p.Text != "" {
				fmt.Fprintf(out, "      part %d text  : %s\n", i+1, p.Text)
			}
			if p.Type != "" {
				fmt.Fprintf(out, "      part %d type  : %s\n", i+1, p.Type)
			}
		}
	}
	if len(snap.Artifacts) > 0 {
		fmt.Fprintf(out, "    %s\n", dim("artifacts:"))
		for _, a := range snap.Artifacts {
			fmt.Fprintf(out, "      %s %s\n", cyan("•"), a.Name)
			for i, p := range a.Parts {
				if p.Text != "" {
					fmt.Fprintf(out, "        part %d: %s\n", i+1, p.Text)
				}
			}
		}
	}
	if len(snap.History) > 0 {
		fmt.Fprintf(out, "    %s\n", dim("history:"))
		for i, m := range snap.History {
			fmt.Fprintf(out, "      %d. %s\n", i+1, m.Role)
			for _, p := range m.Parts {
				if p.Text != "" {
					fmt.Fprintf(out, "         - %s\n", p.Text)
				}
			}
		}
	}
	return nil
}

// --- Task selection helper ---------------------------------------------------

type taskEntry struct {
	HubTaskID  string `json:"hub_task_id"`
	UpstreamID string `json:"upstream_id"`
	State      string `json:"state"`
	ContextID  string `json:"context_id"`
	UpdatedAt  string `json:"updated_at"`
}

func selectTask(c *adminClient, includeRecent bool) (*taskEntry, error) {
	path := "/admin/tasks?limit=20"
	if includeRecent {
		path += "&recent=true"
	}
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	var data struct {
		Items []taskEntry `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if len(data.Items) == 0 {
		fmt.Println("No tasks found.")
		return nil, nil
	}

	opts := make([]string, len(data.Items))
	for i, t := range data.Items {
		opts[i] = fmt.Sprintf("%s  %s  %s  %s",
			padCell(shortID(t.HubTaskID), 10),
			padCell(statusDot(t.State), 14),
			padCell(shortID(t.UpstreamID), 10),
			formatTimeShort(t.UpdatedAt))
	}
	idx := readChoice("Select task:", opts)
	if idx < 0 {
		return nil, nil
	}
	return &data.Items[idx], nil
}

func resolveTaskID(c *adminClient, taskID string) (string, error) {
	if taskID == "" {
		return "", nil
	}
	if len(taskID) >= 8 && strings.Contains(taskID, "-") {
		return taskID, nil
	}

	path := "/admin/tasks?limit=20&recent=true"
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpErr(resp)
	}

	var data struct {
		Items []taskEntry `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}

	for _, item := range data.Items {
		if strings.HasPrefix(item.HubTaskID, taskID) {
			return item.HubTaskID, nil
		}
	}

	return taskID, nil
}
