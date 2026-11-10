// Package events extracts event bus bindings from source: the topics and
// queues a service publishes to (EdgePublishes) or consumes from (EdgeConsumes).
//
// # Why this is broker-specific
//
// Event-driven architectures connect decoupled microservices through message
// buses without direct HTTP connections. A service that emits a `user.created`
// event to Kafka, and five services that consume it, form a real dependency
// that static call graphs cannot see unless event bus bindings are extracted.
//
// RepoCortex recognizes the patterns across the major event platforms:
//   - Apache Kafka (Sarama, librdkafka, Spring-Kafka, KafkaJS, Confluent)
//   - RabbitMQ / AMQP (streadway/amqp, pika, Spring-AMQP)
//   - AWS SQS & SNS (boto3, aws-sdk-go, Spring Cloud AWS)
//   - Google Cloud Pub/Sub (cloud.google.com/go/pubsub, google-cloud-pubsub)
package events

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Broker names the message broker technology.
type Broker string

const (
	BrokerKafka    Broker = "kafka"
	BrokerRabbitMQ Broker = "rabbitmq"
	BrokerSQS      Broker = "sqs"
	BrokerSNS      Broker = "sns"
	BrokerPubSub   Broker = "pubsub"
)

// Direction distinguishes publishing from consuming.
type Direction string

const (
	DirectionPublish Direction = "publish"
	DirectionConsume Direction = "consume"
)

// EventBinding represents one discovered event bus interaction.
type EventBinding struct {
	Broker       Broker           `json:"broker"`
	Direction    Direction        `json:"direction"` // publish | consume
	Topic        string           `json:"topic"`     // topic, queue, or exchange name
	Function     string           `json:"function,omitempty"`
	SrcSymbolFQN string           `json:"src_symbol_fqn,omitempty"`
	Line         int              `json:"line"`
	Confidence   model.Confidence `json:"confidence"`
	EdgeKind     model.EdgeKind   `json:"edge_kind"` // PUBLISHES | CONSUMES
}

// SupportedBrokers lists the event brokers RepoCortex inspects.
func SupportedBrokers() []Broker {
	return []Broker{
		BrokerKafka,
		BrokerRabbitMQ,
		BrokerSQS,
		BrokerSNS,
		BrokerPubSub,
	}
}

var (
	// Kafka patterns
	reSpringKafkaListener = regexp.MustCompile(`@KafkaListener\s*\(\s*topics\s*=\s*["']([^"']+)["']`)
	reSpringKafkaSend     = regexp.MustCompile(`kafkaTemplate\.send\s*\(\s*["']([^"']+)["']`)
	reSaramaProducer      = regexp.MustCompile(`(?:ProducerMessage\s*\{[^}]*Topic:|Producer[^{]*\{[^}]*Topic:)\s*["']([^"']+)["']`)
	reKafkaGoWriter       = regexp.MustCompile(`(?:kafka\.Writer\s*\{[^}]*Topic:|kafka\.WriterConfig\s*\{[^}]*Topic:)\s*["']([^"']+)["']`)
	reKafkaGoReader       = regexp.MustCompile(`(?:kafka\.ReaderConfig\s*\{[^}]*Topic:|kafka\.NewReader\s*\([^)]*Topic:|Consumer[^{]*\{[^}]*Topic:)\s*["']([^"']+)["']`)
	reKafkaJSSend         = regexp.MustCompile(`(?:producer|producer\(\))\.send\s*\(\s*\{[^}]*topic:\s*["']([^"']+)["']`)
	reKafkaJSSubscribe    = regexp.MustCompile(`consumer\.subscribe\s*\(\s*\{[^}]*topic:\s*["']([^"']+)["']`)
	rePythonKafkaSend     = regexp.MustCompile(`(?:producer|client)\.send\s*\(\s*["']([^"']+)["']`)
	reProducerCtx         = regexp.MustCompile(`(?i)(?:ProducerMessage|kafka\.Writer|Producer)`)
	reConsumerCtx         = regexp.MustCompile(`(?i)(?:Consumer|ReaderConfig|kafka\.Reader)`)
	reTopicField          = regexp.MustCompile(`(?i)\b(?:Topic)\s*:\s*["']([^"']+)["']`)

	// RabbitMQ / AMQP patterns
	reSpringRabbitListener = regexp.MustCompile(`@RabbitListener\s*\(\s*queues\s*=\s*["']([^"']+)["']`)
	reRabbitTemplateSend   = regexp.MustCompile(`rabbitTemplate\.convertAndSend\s*\(\s*["']([^"']+)["']`)
	reAmqpPublish          = regexp.MustCompile(`(?:\.Publish|\.PublishWithContext)\s*\(\s*["']([^"']*)["']\s*,\s*["']([^"']+)["']`)
	reAmqpConsume          = regexp.MustCompile(`\.Consume\s*\(\s*["']([^"']+)["']`)
	rePikaPublish          = regexp.MustCompile(`basic_publish\s*\([^)]*exchange\s*=\s*["']([^"']+)["']`)
	rePikaConsume          = regexp.MustCompile(`basic_consume\s*\([^)]*queue\s*=\s*["']([^"']+)["']`)

	// AWS SQS / SNS patterns
	reSpringSqsListener = regexp.MustCompile(`@SqsListener\s*\(\s*["']([^"']+)["']`)
	reSnsPublish        = regexp.MustCompile(`(?:snsClient\.publish|sns\.publish)\s*\([^)]*TopicArn\s*[:=]\s*["']([^"']+)["']`)
	reSqsSend           = regexp.MustCompile(`(?:sqsClient\.sendMessage|sqs\.send_message|sqs\.SendMessage)\s*\([^)]*(?:QueueUrl|QueueName)\s*[:=]\s*["']([^"']+)["']`)

	// Google Cloud Pub/Sub patterns
	rePubSubPublish = regexp.MustCompile(`(?:\.Topic\s*\(\s*["']([^"']+)["']\s*\)\.Publish|publisher\.publish\s*\(\s*["']([^"']+)["'])`)
	rePubSubConsume = regexp.MustCompile(`(?:\.Subscription\s*\(\s*["']([^"']+)["']\s*\)\.Receive|subscriber\.subscribe\s*\(\s*["']([^"']+)["'])`)

	// Enclosing function helper
	reEnclosingFunc = regexp.MustCompile(`(?:func\s+(?:\([^)]+\)\s+)?([A-Za-z0-9_]+)|def\s+([A-Za-z0-9_]+)|public\s+[^\(]+\s+([A-Za-z0-9_]+)\s*\()`)
)

// Extract finds all event bus publications and subscriptions in a source file.
func Extract(filePath string, src []byte) []EventBinding {
	if len(src) == 0 {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go", ".java", ".py", ".ts", ".js":
		// Process supported languages
	default:
		return nil
	}

	lines := strings.Split(string(src), "\n")
	var bindings []EventBinding
	currentFunc := ""
	currentCtx := ""

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Track current function scope
		if m := reEnclosingFunc.FindStringSubmatch(trimmed); len(m) > 0 {
			for j := 1; j < len(m); j++ {
				if m[j] != "" {
					currentFunc = m[j]
					break
				}
			}
		}

		if reProducerCtx.MatchString(trimmed) {
			currentCtx = "producer"
		}
		if reConsumerCtx.MatchString(trimmed) {
			currentCtx = "consumer"
		}

		// 1. Kafka Checks
		if m := reSpringKafkaListener.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
		if m := reSpringKafkaSend.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := reSaramaProducer.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := reKafkaGoWriter.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := reKafkaGoReader.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
		if currentCtx != "" {
			if m := reTopicField.FindStringSubmatch(trimmed); len(m) > 1 {
				dir := DirectionPublish
				edge := model.EdgePublishes
				if currentCtx == "consumer" {
					dir = DirectionConsume
					edge = model.EdgeConsumes
				}
				bindings = append(bindings, EventBinding{
					Broker:     BrokerKafka,
					Direction:  dir,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   edge,
				})
			}
		}
		if strings.Contains(trimmed, "}") {
			currentCtx = ""
		}
		if m := reKafkaJSSend.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := reKafkaJSSubscribe.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
		if m := rePythonKafkaSend.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerKafka,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceMedium,
				EdgeKind:   model.EdgePublishes,
			})
		}

		// 2. RabbitMQ / AMQP Checks
		if m := reSpringRabbitListener.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerRabbitMQ,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
		if m := reRabbitTemplateSend.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerRabbitMQ,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := reAmqpConsume.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerRabbitMQ,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
		if m := reAmqpPublish.FindStringSubmatch(trimmed); len(m) > 2 {
			topic := m[1]
			if topic == "" {
				topic = m[2]
			}
			bindings = append(bindings, EventBinding{
				Broker:     BrokerRabbitMQ,
				Direction:  DirectionPublish,
				Topic:      topic,
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := rePikaPublish.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerRabbitMQ,
				Direction:  DirectionPublish,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := rePikaConsume.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerRabbitMQ,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}

		// 3. AWS SQS / SNS Checks
		if m := reSpringSqsListener.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerSQS,
				Direction:  DirectionConsume,
				Topic:      m[1],
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
		if m := reSqsSend.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerSQS,
				Direction:  DirectionPublish,
				Topic:      cleanQueueName(m[1]),
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := reSnsPublish.FindStringSubmatch(trimmed); len(m) > 1 {
			bindings = append(bindings, EventBinding{
				Broker:     BrokerSNS,
				Direction:  DirectionPublish,
				Topic:      cleanTopicArn(m[1]),
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}

		// 4. GCP Pub/Sub Checks
		if m := rePubSubPublish.FindStringSubmatch(trimmed); len(m) > 1 {
			topic := m[1]
			if topic == "" && len(m) > 2 {
				topic = m[2]
			}
			bindings = append(bindings, EventBinding{
				Broker:     BrokerPubSub,
				Direction:  DirectionPublish,
				Topic:      topic,
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgePublishes,
			})
		}
		if m := rePubSubConsume.FindStringSubmatch(trimmed); len(m) > 1 {
			sub := m[1]
			if sub == "" && len(m) > 2 {
				sub = m[2]
			}
			bindings = append(bindings, EventBinding{
				Broker:     BrokerPubSub,
				Direction:  DirectionConsume,
				Topic:      sub,
				Function:   currentFunc,
				Line:       lineNum,
				Confidence: model.ConfidenceHigh,
				EdgeKind:   model.EdgeConsumes,
			})
		}
	}

	return bindings
}

func cleanQueueName(urlOrName string) string {
	parts := strings.Split(urlOrName, "/")
	return parts[len(parts)-1]
}

func cleanTopicArn(arn string) string {
	parts := strings.Split(arn, ":")
	return parts[len(parts)-1]
}
