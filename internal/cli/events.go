package cli

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

func eventsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Inspect the event log",
	}

	var limit int
	var eventType string
	list := &cobra.Command{
		Use:   "list",
		Short: "List recent events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client(cmd, g)
			if err != nil {
				return err
			}
			q := url.Values{}
			if limit > 0 {
				q.Set("limit", fmt.Sprintf("%d", limit))
			}
			if eventType != "" {
				q.Set("type", eventType)
			}
			path := "/v1/events"
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			out, err := c.Do(ctx, "GET", path, nil)
			if err != nil {
				return err
			}
			fprintJSON(cmd.OutOrStdout(), g.color, out)
			return nil
		},
	}
	list.Flags().IntVar(&limit, "limit", 0, "max events to return")
	list.Flags().StringVar(&eventType, "type", "", "exact event type filter (e.g. customer.created)")

	retrieve := &cobra.Command{
		Use:   "retrieve <event-id>",
		Short: "Fetch one event by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client(cmd, g)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			out, err := c.Do(ctx, "GET", "/v1/events/"+args[0], nil)
			if err != nil {
				return err
			}
			fprintJSON(cmd.OutOrStdout(), g.color, out)
			return nil
		},
	}

	cmd.AddCommand(list, retrieve)
	return cmd
}
