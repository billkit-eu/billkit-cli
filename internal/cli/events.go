package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
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
	var startingAfter string
	list := &cobra.Command{
		Use:   "list",
		Short: "List recent events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Before any credential is read, like every other flag check in
			// this CLI. `listen --events` already fails locally on a typo;
			// without this, the same typo here quietly returned an empty page
			// that reads as "nothing happened" rather than "no such type".
			if unknown := validateEventTypes(eventType); len(unknown) > 0 {
				return fmt.Errorf(
					"unknown event type(s) in --type: %s\n"+
						"Omit --type to list every event, or run "+
						"`billkit api GET /v1/webhook_endpoints/event_types` for the %d available",
					strings.Join(unknown, ", "), len(knownEventTypes),
				)
			}

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
			if startingAfter != "" {
				q.Set("starting_after", startingAfter)
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
	list.Flags().StringVar(&startingAfter, "starting-after", "", "cursor: return events after this id")

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
			out, err := c.Do(ctx, "GET", "/v1/events/"+url.PathEscape(args[0]), nil)
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
