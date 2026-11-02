package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your submissions (admins can list everyone's with --all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			path := "/api/submissions"
			if all {
				path = "/api/admin/submissions"
			}
			var out struct {
				Submissions []submission `json:"submissions"`
			}
			if err := newClient(cfg.Server, cfg.Token).do(ctx, http.MethodGet, path, nil, &out); err != nil {
				return err
			}
			printSubmissions(cmd, out.Submissions, all)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every user's submissions (admins only)")
	return cmd
}

func printSubmissions(cmd *cobra.Command, subs []submission, showOwner bool) {
	if len(subs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No submissions.")
		return
	}
	// newest first, like the website
	sortByNewest(subs)
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	header := "ID\tTYPE\tDATE\t"
	if showOwner {
		header += "OWNER\t"
	}
	fmt.Fprintln(tw, header+"URL\tDESCRIPTION")
	for _, s := range subs {
		row := fmt.Sprintf("%s\t%s\t%s\t", s.ID, s.Type, shortDate(s.Timestamp))
		if showOwner {
			row += orDash(s.OwnerUsername) + "\t"
		}
		fmt.Fprintln(tw, row+orDash(s.URL)+"\t"+oneLine(s.Description, 60))
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortDate(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// oneLine collapses whitespace and truncates to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func sortByNewest(subs []submission) {
	sort.SliceStable(subs, func(i, j int) bool { return subs[i].Timestamp > subs[j].Timestamp })
}
