package cli

import (
	"net/url"
	"strings"

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
		Short: "List recent events, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Before any credential is read, like every other flag check in
			// this CLI. `listen --events` already fails locally on a typo;
			// without this, the same typo here quietly returned an empty page
			// that reads as "nothing happened" rather than "no such type".
			if unknown := validateEventTypes(eventType); len(unknown) > 0 {
				return usageErrorf(
					"unknown event type(s) in --type: %s\n"+
						"Omit --type to list every event, or run "+
						"`billkit api GET /v1/webhook_endpoints/event_types` for the %d available",
					strings.Join(unknown, ", "), len(knownEventTypes),
				)
			}
			q := url.Values{}
			if err := setLimit(cmd, q, limit); err != nil {
				return err
			}
			if eventType != "" {
				q.Set("type", eventType)
			}
			if startingAfter != "" {
				q.Set("starting_after", startingAfter)
			}
			return getAndPrint(cmd, g, withQuery("/v1/events", q))
		},
	}
	list.Flags().IntVar(&limit, "limit", 0, limitUsage("events"))
	list.Flags().StringVar(&eventType, "type", "", "event type filter, or a comma-separated list of them (e.g. invoice.paid,invoice.voided)")
	list.Flags().StringVar(&startingAfter, "starting-after", "", "cursor: return events after this id")

	retrieve := &cobra.Command{
		Use:   "retrieve <event-id>",
		Short: "Fetch one event by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return getAndPrint(cmd, g, "/v1/events/"+url.PathEscape(args[0]))
		},
	}

	cmd.AddCommand(list, retrieve)
	return cmd
}
