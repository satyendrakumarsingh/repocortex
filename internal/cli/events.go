package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/events"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/spf13/cobra"
)

func newEventsCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Inspect event bus topology (Kafka, RabbitMQ, SQS, SNS, Pub/Sub)",
		Long: `Reports the distributed event bus topology across message brokers.
Discovers topics, queues, exchanges, and which functions publish to or consume from them.

Supported brokers:
  - Apache Kafka (Sarama, kafka-go, Spring-Kafka, KafkaJS, kafka-python)
  - RabbitMQ / AMQP (streadway/amqp, pika, Spring-AMQP)
  - AWS SQS & SNS (aws-sdk-go, boto3, Spring Cloud AWS)
  - Google Cloud Pub/Sub (cloud.google.com/go/pubsub, google-cloud-pubsub)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			report, err := s.reg.EventTopologyReport(ctx, s.runID)
			if err != nil {
				return err
			}

			if g.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Supported event brokers: Kafka, RabbitMQ, AWS SQS/SNS, GCP Pub/Sub")
			fmt.Fprintln(out)

			if len(report) == 0 {
				fmt.Fprintln(out, "No event bus publishers or consumers found.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "BROKER\tTOPIC\tPRODUCERS\tCONSUMERS\tFQN")
			for _, top := range report {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n",
					top.Broker, top.Topic, len(top.Producers), len(top.Consumers), top.FQN)
			}
			_ = tw.Flush()

			fmt.Fprintf(out, "\n%d event topic(s) discovered.\n", len(report))
			return nil
		},
	}

	cmd.AddCommand(newEventsTopicCmd(g))
	return cmd
}

func newEventsTopicCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "topic <name>",
		Short: "Show publishers and consumers for a single event topic",
		Long:  `Lists all functions and methods that publish to or consume from a specific event topic.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.ToLower(strings.TrimSpace(args[0]))
			if query == "" {
				return errors.New("topic name must not be empty")
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			report, err := s.reg.EventTopologyReport(ctx, s.runID)
			if err != nil {
				return err
			}

			var target *registry.EventTopology
			for i := range report {
				if strings.ToLower(report[i].Topic) == query || report[i].FQN == query {
					target = &report[i]
					break
				}
			}

			if target == nil {
				return fmt.Errorf("no event topic %q found; run 'cortex events' to see discovered topics", query)
			}

			if g.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(target)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Topic:  %s\nBroker: %s\nFQN:    %s\n\n", target.Topic, target.Broker, target.FQN)

			if len(target.Producers) > 0 {
				fmt.Fprintf(out, "Producers (%d):\n", len(target.Producers))
				tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "  FUNCTION\tLOCATION\tCONFIDENCE\tDETAIL")
				for _, p := range target.Producers {
					fn := p.SymbolName
					if fn == "" {
						fn = p.SymbolFQN
					}
					loc := fmt.Sprintf("%s:%d", p.FilePath, p.Line)
					fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", fn, loc, p.Confidence, p.Detail)
				}
				_ = tw.Flush()
				fmt.Fprintln(out)
			} else {
				fmt.Fprintln(out, "Producers: none (external publisher)")
			}

			if len(target.Consumers) > 0 {
				fmt.Fprintf(out, "Consumers (%d):\n", len(target.Consumers))
				tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "  FUNCTION\tLOCATION\tCONFIDENCE\tDETAIL")
				for _, c := range target.Consumers {
					fn := c.SymbolName
					if fn == "" {
						fn = c.SymbolFQN
					}
					loc := fmt.Sprintf("%s:%d", c.FilePath, c.Line)
					fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", fn, loc, c.Confidence, c.Detail)
				}
				_ = tw.Flush()
			} else {
				fmt.Fprintln(out, "Consumers: none (external consumer)")
			}

			return nil
		},
	}
}

// Ensure events package is referenced
var _ = events.SupportedBrokers
