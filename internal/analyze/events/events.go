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
//   - ActiveMQ over STOMP or JMS (go-stomp, stomp.py, @stomp/stompjs, Spring JMS)
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
	BrokerActiveMQ Broker = "activemq"
	BrokerCelery   Broker = "celery"
	BrokerBullMQ   Broker = "bullmq"
	BrokerTemporal Broker = "temporal"
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
		BrokerActiveMQ,
		BrokerCelery,
		BrokerBullMQ,
		BrokerTemporal,
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
	// ActiveMQ over STOMP (go-stomp, stomp.py, stompjs) and JMS (Spring).
	// STOMP call sites are recognised only with a literal /queue/ or /topic/
	// destination, or a destination= keyword: Send and Subscribe are names too
	// common to claim on their own.
	reStompGoSend      = regexp.MustCompile(`\.Send\s*\(\s*"(/(?:queue|topic)/[^"]+)"`)
	reStompGoSubscribe = regexp.MustCompile(`\.Subscribe\s*\(\s*"(/(?:queue|topic)/[^"]+)"`)
	reStompPySend      = regexp.MustCompile(`\.send\s*\([^)]*destination\s*=\s*["']([^"']+)["']`)
	reStompPySubscribe = regexp.MustCompile(`\.subscribe\s*\([^)]*destination\s*=\s*["']([^"']+)["']`)
	reStompJSPublish   = regexp.MustCompile(`\.publish\s*\(\s*\{[^}]*destination\s*:\s*["']([^"']+)["']`)
	reStompSubscribe   = regexp.MustCompile(`\.subscribe\s*\(\s*["'](/(?:queue|topic)/[^"']+)["']`)
	reJmsListener      = regexp.MustCompile(`@JmsListener\s*\(\s*destination\s*=\s*"([^"]+)"`)
	reJmsTemplateSend  = regexp.MustCompile(`jmsTemplate\.(?:convertAndSend|send)\s*\(\s*"([^"]+)"`)

	// Celery patterns (Python async task queue)
	// @shared_task, and @<app>.task on any Celery app variable: app,
	// celery, celery_app.
	reCeleryTaskDec  = regexp.MustCompile(`^@((?:[A-Za-z_]\w*\.)+task|shared_task)\b(?:\((.*)\))?`)
	reCeleryTaskName = regexp.MustCompile(`\bname\s*=\s*["']([^"']+)["']`)
	reCeleryDelay    = regexp.MustCompile(`\b([A-Za-z0-9_]+)\.(?:delay|apply_async)\s*\(`)
	reCelerySendTask = regexp.MustCompile(`\b(?:app|celery)\.send_task\s*\(\s*["']([^"']+)["']`)

	// BullMQ patterns (Node / TS redis task queue)
	reBullWorker  = regexp.MustCompile(`new\s+Worker\s*(?:<[^>]+>)?\s*\(\s*['"]([^'"]+)['"]`)
	reBullProcess = regexp.MustCompile(`\b([a-zA-Z_$][\w$]*)\.process\s*\(\s*['"]([^'"]+)['"]`)
	// A file uses Bull when it imports it; the word "bull" alone is in
	// "bullet" and "bulletin".
	reBullImport = regexp.MustCompile(`(?:from\s+|require\s*\(\s*)['"](?:bullmq|bull|@nestjs/bull|@nestjs/bullmq)['"]`)
	// The variables a file holds queues in: `const emails = new Queue(...)`,
	// `this.emails = new Queue(...)`, `@InjectQueue('x') private emails`.
	reBullQueueVar    = regexp.MustCompile(`\b([a-zA-Z_$][\w$]*)\s*(?::\s*[\w$<>, ]+)?=\s*new\s+Queue\b`)
	reBullInjectQueue = regexp.MustCompile(`@InjectQueue\s*\([^)]*\)\s*(?:(?:private|public|protected|readonly)\s+)*([a-zA-Z_$][\w$]*)`)
	reBullAdd         = regexp.MustCompile(`\b([a-zA-Z_$][\w$]*)\.add(?:Bulk)?\s*\(\s*['"]([^'"]+)['"]`)

	// Temporal patterns (Go / TS / Python workflow engine)
	reTemporalGoRegister = regexp.MustCompile(`\b(?:w|worker)\.Register(?:Workflow|Activity)(?:WithOptions)?\s*\(\s*([A-Za-z0-9_]+)`)
	reTemporalGoExecute  = regexp.MustCompile(`\bworkflow\.(?:ExecuteActivity|ExecuteChildWorkflow)\s*\([^,]+,\s*([A-Za-z0-9_]+)`)
	reTemporalPyDefn     = regexp.MustCompile(`^@(workflow|activity)\.defn\b(?:\((.*)\))?`)
	reTemporalPyName     = regexp.MustCompile(`\bname\s*=\s*["']([^"']+)["']`)
	rePyClass            = regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`)
	// client.execute_workflow(GreetingWorkflow.run, ...) and start_workflow,
	// by the class or by its registered name.
	reTemporalPyClient  = regexp.MustCompile(`\.(?:execute|start)_workflow\s*\(\s*(?:["']([^"']+)["']|([A-Za-z0-9_]+))`)
	reTemporalPyExecute = regexp.MustCompile(`\bworkflow\.execute_(?:activity|child_workflow)\s*\(\s*([A-Za-z0-9_]+)`)
	reTemporalTSExecute = regexp.MustCompile(`(?:workflowClient\.start|client\.workflow\.start)\s*\(\s*([A-Za-z0-9_]+)`)

	reEnclosingFunc = regexp.MustCompile(`(?:func\s+(?:\([^)]+\)\s+)?([A-Za-z0-9_]+)|def\s+([A-Za-z0-9_]+)|public\s+[^\(]+\s+([A-Za-z0-9_]+)\s*\()`)
)

// bullQueueVars is every variable the file holds a Bull queue in.
func bullQueueVars(src string) map[string]bool {
	out := map[string]bool{}
	for _, re := range []*regexp.Regexp{reBullQueueVar, reBullInjectQueue} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// isBullQueue reports whether a receiver is a queue: one the file assigns a
// Queue to, or one named as a queue. A set, a form or URLSearchParams also
// has add("x"), and none of them is a job.
func isBullQueue(receiver string, vars map[string]bool) bool {
	return vars[receiver] || strings.Contains(strings.ToLower(receiver), "queue")
}

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

	// Which broker a generic producer.send("X") call talks to is a property
	// of the file, not of the call: a file that imports a STOMP or ActiveMQ
	// client and no Kafka one is sending to ActiveMQ. Without this every such
	// call was filed under Kafka, and on a real repository all three "Kafka
	// topics" were ActiveMQ queue keys with no consumers.
	lowerSrc := strings.ToLower(string(src))
	usesKafka := strings.Contains(lowerSrc, "kafka")
	usesActiveMQ := strings.Contains(lowerSrc, "activemq") || strings.Contains(lowerSrc, "stomp")
	usesCelery := strings.Contains(lowerSrc, "celery") || strings.Contains(lowerSrc, "shared_task")
	usesBull := reBullImport.MatchString(string(src))
	var queueVars map[string]bool
	if usesBull {
		queueVars = bullQueueVars(string(src))
	}
	usesTemporal := strings.Contains(lowerSrc, "temporal") || strings.Contains(lowerSrc, "cadence")
	genericBroker := BrokerKafka
	if usesActiveMQ && !usesKafka {
		genericBroker = BrokerActiveMQ
	}
	currentCtx := ""
	// A task or workflow decorator names the definition that follows it, so
	// the binding waits for that definition's line and name.
	pendingCelery, pendingCeleryName := false, ""
	pendingTemporal, pendingTemporalName := false, ""

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
			if pendingCelery && currentFunc != "" {
				// task.delay() names the function and send_task("x") the
				// registered name; the task consumes both.
				bindings = append(bindings, celeryConsume(currentFunc, currentFunc, lineNum))
				if pendingCeleryName != "" && pendingCeleryName != currentFunc {
					bindings = append(bindings, celeryConsume(pendingCeleryName, currentFunc, lineNum))
				}
				pendingCelery, pendingCeleryName = false, ""
			}
			if pendingTemporal && currentFunc != "" {
				bindings = append(bindings, temporalConsume(pendingTemporalName, currentFunc, lineNum))
				pendingTemporal, pendingTemporalName = false, ""
			}
		} else if pendingTemporal && ext == ".py" {
			// @workflow.defn decorates a class; the workflow is the class.
			if m := rePyClass.FindStringSubmatch(trimmed); m != nil {
				currentFunc = m[1]
				bindings = append(bindings, temporalConsume(pendingTemporalName, m[1], lineNum))
				pendingTemporal, pendingTemporalName = false, ""
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
		if m := rePythonKafkaSend.FindStringSubmatch(trimmed); len(m) > 1 && !reStompPySend.MatchString(trimmed) {
			bindings = append(bindings, EventBinding{
				Broker:     genericBroker,
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

		// 5. ActiveMQ: STOMP and JMS
		amq := func(re *regexp.Regexp, dir Direction, kind model.EdgeKind) {
			if m := re.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker: BrokerActiveMQ, Direction: dir, Topic: m[1], Function: currentFunc,
					Line: lineNum, Confidence: model.ConfidenceHigh, EdgeKind: kind,
				})
			}
		}
		amq(reStompGoSend, DirectionPublish, model.EdgePublishes)
		amq(reStompGoSubscribe, DirectionConsume, model.EdgeConsumes)
		amq(reStompPySend, DirectionPublish, model.EdgePublishes)
		amq(reStompPySubscribe, DirectionConsume, model.EdgeConsumes)
		amq(reStompJSPublish, DirectionPublish, model.EdgePublishes)
		if !reStompPySubscribe.MatchString(trimmed) {
			amq(reStompSubscribe, DirectionConsume, model.EdgeConsumes)
		}
		amq(reJmsListener, DirectionConsume, model.EdgeConsumes)
		amq(reJmsTemplateSend, DirectionPublish, model.EdgePublishes)

		// 6. Celery task queues (Python)
		if usesCelery && ext == ".py" {
			if m := reCeleryTaskDec.FindStringSubmatch(trimmed); len(m) > 0 {
				pendingCelery, pendingCeleryName = true, ""
				if len(m) > 2 && m[2] != "" {
					if nm := reCeleryTaskName.FindStringSubmatch(m[2]); len(nm) > 1 {
						pendingCeleryName = nm[1]
					}
				}
			}
			if m := reCeleryDelay.FindStringSubmatch(trimmed); len(m) > 1 {
				task := m[1]
				if task != "time" && task != "asyncio" && task != "sleep" {
					bindings = append(bindings, EventBinding{
						Broker:     BrokerCelery,
						Direction:  DirectionPublish,
						Topic:      task,
						Function:   currentFunc,
						Line:       lineNum,
						Confidence: model.ConfidenceHigh,
						EdgeKind:   model.EdgePublishes,
					})
				}
			}
			if m := reCelerySendTask.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerCelery,
					Direction:  DirectionPublish,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgePublishes,
				})
			}
		}

		// 7. BullMQ task queues (JS/TS)
		if usesBull && (ext == ".ts" || ext == ".js") {
			if m := reBullWorker.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerBullMQ,
					Direction:  DirectionConsume,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgeConsumes,
				})
			}
			if m := reBullProcess.FindStringSubmatch(trimmed); len(m) > 2 && isBullQueue(m[1], queueVars) {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerBullMQ,
					Direction:  DirectionConsume,
					Topic:      m[2],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgeConsumes,
				})
			}
			if m := reBullAdd.FindStringSubmatch(trimmed); len(m) > 2 {
				receiver := m[1]
				job := m[2]
				if isBullQueue(receiver, queueVars) {
					bindings = append(bindings, EventBinding{
						Broker:     BrokerBullMQ,
						Direction:  DirectionPublish,
						Topic:      job,
						Function:   currentFunc,
						Line:       lineNum,
						Confidence: model.ConfidenceHigh,
						EdgeKind:   model.EdgePublishes,
					})
				}
			}
		}

		// 8. Temporal workflows and activities
		if usesTemporal {
			if m := reTemporalGoRegister.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerTemporal,
					Direction:  DirectionConsume,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgeConsumes,
				})
			}
			if m := reTemporalGoExecute.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerTemporal,
					Direction:  DirectionPublish,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgePublishes,
				})
			}
			if m := reTemporalPyDefn.FindStringSubmatch(trimmed); m != nil && ext == ".py" {
				pendingTemporal, pendingTemporalName = true, ""
				if nm := reTemporalPyName.FindStringSubmatch(m[2]); nm != nil {
					pendingTemporalName = nm[1]
				}
			}
			if m := reTemporalPyClient.FindStringSubmatch(trimmed); m != nil && ext == ".py" {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerTemporal,
					Direction:  DirectionPublish,
					Topic:      m[1] + m[2],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgePublishes,
				})
			}
			if m := reTemporalPyExecute.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerTemporal,
					Direction:  DirectionPublish,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgePublishes,
				})
			}
			if m := reTemporalTSExecute.FindStringSubmatch(trimmed); len(m) > 1 {
				bindings = append(bindings, EventBinding{
					Broker:     BrokerTemporal,
					Direction:  DirectionPublish,
					Topic:      m[1],
					Function:   currentFunc,
					Line:       lineNum,
					Confidence: model.ConfidenceHigh,
					EdgeKind:   model.EdgePublishes,
				})
			}
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

func celeryConsume(topic, fn string, line int) EventBinding {
	return EventBinding{
		Broker: BrokerCelery, Direction: DirectionConsume, Topic: topic, Function: fn,
		Line: line, Confidence: model.ConfidenceHigh, EdgeKind: model.EdgeConsumes,
	}
}

// temporalConsume is a workflow or activity definition, under its registered
// name when the decorator gives one.
func temporalConsume(name, fn string, line int) EventBinding {
	topic := fn
	if name != "" {
		topic = name
	}
	return EventBinding{
		Broker: BrokerTemporal, Direction: DirectionConsume, Topic: topic, Function: fn,
		Line: line, Confidence: model.ConfidenceHigh, EdgeKind: model.EdgeConsumes,
	}
}
