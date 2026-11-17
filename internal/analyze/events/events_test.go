package events

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestExtractNegativeCases(t *testing.T) {
	// 1. Empty source
	if b := Extract("service.go", nil); len(b) != 0 {
		t.Errorf("expected 0 bindings on nil src, got %d", len(b))
	}

	// 2. Unsupported file extension
	cssCode := []byte(`.publish-button { color: red; }`)
	if b := Extract("styles.css", cssCode); len(b) != 0 {
		t.Errorf("expected 0 bindings on .css file, got %d", len(b))
	}

	// 3. Regular code with variable names matching words like publish or topic
	plainGo := []byte(`package main

func DoWork() {
	var topic = "some-string"
	var publish = true
	if publish {
		println(topic)
	}
}
`)
	if b := Extract("main.go", plainGo); len(b) != 0 {
		t.Errorf("expected 0 bindings on plain code, got %d: %+v", len(b), b)
	}
}

func TestExtractPositiveKafka(t *testing.T) {
	// Java Kafka Spring
	javaSrc := []byte(`package com.example.service;

@Service
public class OrderService {

    @KafkaListener(topics = "orders.v1")
    public void handleOrder(OrderEvent event) {
        process(event);
    }

    public void createOrder(Order order) {
        kafkaTemplate.send("orders.created", order);
    }
}
`)

	bindings := Extract("OrderService.java", javaSrc)
	if len(bindings) != 2 {
		t.Fatalf("expected 2 Kafka bindings, got %d", len(bindings))
	}

	// First is consume orders.v1
	b0 := bindings[0]
	if b0.Broker != BrokerKafka || b0.Direction != DirectionConsume || b0.Topic != "orders.v1" {
		t.Errorf("unexpected b0: %+v", b0)
	}
	if b0.EdgeKind != model.EdgeConsumes {
		t.Errorf("expected EdgeConsumes, got %s", b0.EdgeKind)
	}

	// Second is publish orders.created
	b1 := bindings[1]
	if b1.Broker != BrokerKafka || b1.Direction != DirectionPublish || b1.Topic != "orders.created" {
		t.Errorf("unexpected b1: %+v", b1)
	}
	if b1.EdgeKind != model.EdgePublishes {
		t.Errorf("expected EdgePublishes, got %s", b1.EdgeKind)
	}
}

func TestExtractPositiveGoSaramaAndAMQP(t *testing.T) {
	goSrc := []byte(`package queue

func ProduceOrder() {
	msg := &sarama.ProducerMessage{
		Topic: "checkout.events",
		Value: sarama.StringEncoder("test"),
	}
	producer.SendMessage(msg)
}

func PublishRabbit() {
	ch.Publish("amq.direct", "orders.routing.key", false, false, msg)
}

func ConsumeRabbit() {
	msgs, err := ch.Consume("orders.incoming")
}
`)

	bindings := Extract("queue.go", goSrc)
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings, got %d: %+v", len(bindings), bindings)
	}

	// 1. Sarama Kafka
	if bindings[0].Broker != BrokerKafka || bindings[0].Topic != "checkout.events" || bindings[0].Direction != DirectionPublish {
		t.Errorf("unexpected binding 0: %+v", bindings[0])
	}

	// 2. RabbitMQ Publish
	if bindings[1].Broker != BrokerRabbitMQ || bindings[1].Topic != "amq.direct" || bindings[1].Direction != DirectionPublish {
		t.Errorf("unexpected binding 1: %+v", bindings[1])
	}

	// 3. RabbitMQ Consume
	if bindings[2].Broker != BrokerRabbitMQ || bindings[2].Topic != "orders.incoming" || bindings[2].Direction != DirectionConsume {
		t.Errorf("unexpected binding 2: %+v", bindings[2])
	}
}

func TestExtractPositiveAWSAndPubSub(t *testing.T) {
	pySrc := []byte(`import boto3

def send_notification():
    sqs.send_message(QueueUrl='https://sqs.us-east-1.amazonaws.com/12345/user-notifications', MessageBody='hello')

def publish_metrics():
    sns.publish(TopicArn='arn:aws:sns:us-east-1:12345:metric-alerts', Message='alert')
`)

	bindings := Extract("worker.py", pySrc)
	if len(bindings) != 2 {
		t.Fatalf("expected 2 AWS bindings, got %d: %+v", len(bindings), bindings)
	}

	if bindings[0].Broker != BrokerSQS || bindings[0].Topic != "user-notifications" || bindings[0].Direction != DirectionPublish {
		t.Errorf("unexpected binding 0: %+v", bindings[0])
	}

	if bindings[1].Broker != BrokerSNS || bindings[1].Topic != "metric-alerts" || bindings[1].Direction != DirectionPublish {
		t.Errorf("unexpected binding 1: %+v", bindings[1])
	}

	// Test GCP Pub/Sub Go
	gcpSrc := []byte(`package pubsub

func SendEvent() {
	client.Topic("audit-logs").Publish(ctx, msg)
}

func ListenEvent() {
	client.Subscription("audit-sub").Receive(ctx, handler)
}
`)
	gcpBindings := Extract("pubsub.go", gcpSrc)
	if len(gcpBindings) != 2 {
		t.Fatalf("expected 2 GCP PubSub bindings, got %d: %+v", len(gcpBindings), gcpBindings)
	}

	if gcpBindings[0].Broker != BrokerPubSub || gcpBindings[0].Topic != "audit-logs" || gcpBindings[0].Direction != DirectionPublish {
		t.Errorf("unexpected gcp 0: %+v", gcpBindings[0])
	}
	if gcpBindings[1].Broker != BrokerPubSub || gcpBindings[1].Topic != "audit-sub" || gcpBindings[1].Direction != DirectionConsume {
		t.Errorf("unexpected gcp 1: %+v", gcpBindings[1])
	}
}

// find returns the binding for a topic and direction, or nil.
func find(bs []EventBinding, topic string, dir Direction) *EventBinding {
	for i := range bs {
		if bs[i].Topic == topic && bs[i].Direction == dir {
			return &bs[i]
		}
	}
	return nil
}

// A queue producer that talks STOMP to ActiveMQ was reported as a Kafka topic
// with no consumers: the generic producer.send("X") rule assumed Kafka. On a
// real repository all three "Kafka topics" were ActiveMQ queue keys.
func TestPythonProducerIsAttributedToTheBrokerTheFileUses(t *testing.T) {
	stompSrc := []byte("from shared_service.queue.activemq import QueueProducer\n\n" +
		"_producer = QueueProducer()\n\n" +
		"def notify(payload):\n    _producer.send(\"GRN_COMPLETED\", payload)\n")
	b := find(Extract("app/queue/producer.py", stompSrc), "GRN_COMPLETED", DirectionPublish)
	if b == nil || b.Broker != BrokerActiveMQ {
		t.Fatalf("ActiveMQ producer binding = %+v, want broker activemq", b)
	}

	kafkaSrc := []byte("from kafka import KafkaProducer\n\nproducer = KafkaProducer()\n\n" +
		"def emit(v):\n    producer.send(\"orders.created\", v)\n")
	if b := find(Extract("emit.py", kafkaSrc), "orders.created", DirectionPublish); b == nil || b.Broker != BrokerKafka {
		t.Fatalf("Kafka producer binding = %+v, want broker kafka", b)
	}
}

func TestExtractActiveMQStompAndJMS(t *testing.T) {
	goSrc := []byte("package q\n\nimport \"github.com/go-stomp/stomp/v3\"\n\n" +
		"func Pub(conn *stomp.Conn) { _ = conn.Send(\"/queue/pulse.grn.bulk\", \"application/json\", nil) }\n" +
		"func Sub(conn *stomp.Conn) { _, _ = conn.Subscribe(\"/queue/pulse.grn.bulk\", stomp.AckClient) }\n")
	bs := Extract("q.go", goSrc)
	if b := find(bs, "/queue/pulse.grn.bulk", DirectionPublish); b == nil || b.Broker != BrokerActiveMQ {
		t.Errorf("go-stomp Send = %+v", b)
	}
	if b := find(bs, "/queue/pulse.grn.bulk", DirectionConsume); b == nil || b.Broker != BrokerActiveMQ {
		t.Errorf("go-stomp Subscribe = %+v", b)
	}

	pySrc := []byte("import stomp\n\nconn = stomp.Connection()\n" +
		"conn.send(body='x', destination='/queue/orders')\n" +
		"conn.subscribe(destination='/queue/orders', id=1, ack='auto')\n")
	bs = Extract("w.py", pySrc)
	if find(bs, "/queue/orders", DirectionPublish) == nil || find(bs, "/queue/orders", DirectionConsume) == nil {
		t.Errorf("stomp.py bindings = %+v", bs)
	}

	javaSrc := []byte("public class Billing {\n" +
		"    @JmsListener(destination = \"billing.invoices\")\n    public void on(String m) {}\n" +
		"    public void send() { jmsTemplate.convertAndSend(\"billing.invoices\", \"x\"); }\n}\n")
	bs = Extract("Billing.java", javaSrc)
	if b := find(bs, "billing.invoices", DirectionConsume); b == nil || b.Broker != BrokerActiveMQ {
		t.Errorf("JmsListener = %+v", b)
	}
	if b := find(bs, "billing.invoices", DirectionPublish); b == nil || b.Broker != BrokerActiveMQ {
		t.Errorf("jmsTemplate = %+v", b)
	}

	jsSrc := []byte("client.publish({ destination: '/topic/prices', body: '1' });\n" +
		"client.subscribe('/topic/prices', onMessage);\n")
	bs = Extract("feed.js", jsSrc)
	if find(bs, "/topic/prices", DirectionPublish) == nil || find(bs, "/topic/prices", DirectionConsume) == nil {
		t.Errorf("stompjs bindings = %+v", bs)
	}
}

func TestExtractCeleryTaskQueues(t *testing.T) {
	pySrc := []byte(`
from celery import Celery, shared_task

app = Celery('tasks')

@shared_task
def send_email(user_id):
    pass

@app.task(name="billing.generate_invoice")
def make_invoice(order_id):
    pass

def trigger_workflow():
    send_email.delay(42)
    make_invoice.apply_async(args=[101])
    app.send_task("reports.daily_summary", [2026])
`)

	bs := Extract("tasks.py", pySrc)
	if len(bs) < 5 {
		t.Fatalf("expected at least 5 Celery bindings, got %d: %+v", len(bs), bs)
	}

	// 1. send_email (consumed)
	if b := find(bs, "send_email", DirectionConsume); b == nil || b.Broker != BrokerCelery {
		t.Errorf("expected send_email consume binding, got: %+v", b)
	}
	// 2. billing.generate_invoice (consumed)
	if b := find(bs, "billing.generate_invoice", DirectionConsume); b == nil || b.Broker != BrokerCelery {
		t.Errorf("expected billing.generate_invoice consume binding, got: %+v", b)
	}
	// 3. send_email.delay (published)
	if b := find(bs, "send_email", DirectionPublish); b == nil || b.Broker != BrokerCelery {
		t.Errorf("expected send_email publish binding, got: %+v", b)
	}
	// 4. make_invoice.apply_async (published)
	if b := find(bs, "make_invoice", DirectionPublish); b == nil || b.Broker != BrokerCelery {
		t.Errorf("expected make_invoice publish binding, got: %+v", b)
	}
	// 5. app.send_task (published)
	if b := find(bs, "reports.daily_summary", DirectionPublish); b == nil || b.Broker != BrokerCelery {
		t.Errorf("expected reports.daily_summary publish binding, got: %+v", b)
	}
}

func TestExtractBullMQTaskQueues(t *testing.T) {
	tsSrc := []byte(`
import { Queue, Worker } from 'bullmq';

const emailQueue = new Queue('emails');
const emailWorker = new Worker('emails', async (job) => {
    console.log(job.data);
});

async function queueJob() {
    await emailQueue.add('welcome-email', { userId: 123 });
}
`)

	bs := Extract("queue.ts", tsSrc)
	if len(bs) < 2 {
		t.Fatalf("expected at least 2 BullMQ bindings, got %d: %+v", len(bs), bs)
	}

	// Worker consumes from emails queue
	if b := find(bs, "emails", DirectionConsume); b == nil || b.Broker != BrokerBullMQ {
		t.Errorf("expected emails queue consume, got: %+v", b)
	}
	// emailQueue.add publishes welcome-email job
	if b := find(bs, "welcome-email", DirectionPublish); b == nil || b.Broker != BrokerBullMQ {
		t.Errorf("expected welcome-email publish, got: %+v", b)
	}
}

func TestExtractTemporalWorkflowsAndActivities(t *testing.T) {
	// Go Temporal
	goSrc := []byte(`
package temporalwork

import (
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func Register(w worker.Worker) {
	w.RegisterWorkflow(OrderWorkflow)
	w.RegisterActivity(ProcessPaymentActivity)
}

func OrderWorkflow(ctx workflow.Context) error {
	return workflow.ExecuteActivity(ctx, ProcessPaymentActivity).Get(ctx, nil)
}
`)

	bs := Extract("workflow.go", goSrc)
	if len(bs) < 3 {
		t.Fatalf("expected at least 3 Temporal Go bindings, got %d: %+v", len(bs), bs)
	}

	if b := find(bs, "OrderWorkflow", DirectionConsume); b == nil || b.Broker != BrokerTemporal {
		t.Errorf("expected OrderWorkflow consume, got: %+v", b)
	}
	if b := find(bs, "ProcessPaymentActivity", DirectionConsume); b == nil || b.Broker != BrokerTemporal {
		t.Errorf("expected ProcessPaymentActivity consume, got: %+v", b)
	}
	if b := find(bs, "ProcessPaymentActivity", DirectionPublish); b == nil || b.Broker != BrokerTemporal {
		t.Errorf("expected ProcessPaymentActivity publish, got: %+v", b)
	}

	// Python Temporal
	pySrc := []byte(`
from temporalio import workflow, activity

@activity.defn
def send_notification(msg: str):
    pass

@workflow.defn
class MyWorkflow:
    @workflow.run
    async def run(self):
        await workflow.execute_activity(send_notification, "hello")
`)

	pyBs := Extract("temporal_wf.py", pySrc)
	if len(pyBs) < 2 {
		t.Fatalf("expected at least 2 Temporal Python bindings, got %d: %+v", len(pyBs), pyBs)
	}
	if b := find(pyBs, "send_notification", DirectionConsume); b == nil || b.Broker != BrokerTemporal {
		t.Errorf("expected send_notification consume, got: %+v", b)
	}
	if b := find(pyBs, "send_notification", DirectionPublish); b == nil || b.Broker != BrokerTemporal {
		t.Errorf("expected send_notification publish, got: %+v", b)
	}
}

func bindingSet(bs []EventBinding) map[string]bool {
	out := map[string]bool{}
	for _, b := range bs {
		out[string(b.Broker)+" "+string(b.Direction)+" "+b.Topic+" @"+b.Function] = true
	}
	return out
}

func TestCeleryTasksBindToTheDecoratedFunction(t *testing.T) {
	src := `from celery import shared_task
from proj.celery import celery_app

def helper():
    pass

@shared_task(name='billing.charge')
def charge(order_id):
    pass

@celery_app.task
def send_email(to):
    pass

def checkout():
    charge.delay(1)
    celery_app.send_task('billing.charge', args=[1])
`
	got := bindingSet(Extract("tasks.py", []byte(src)))
	for _, want := range []string{
		"celery consume charge @charge",         // what charge.delay() names
		"celery consume billing.charge @charge", // what send_task names
		"celery consume send_email @send_email", // @celery_app.task
		"celery publish charge @checkout",
	} {
		if !got[want] {
			t.Errorf("missing %q; got %v", want, got)
		}
	}
	for b := range got {
		if strings.HasSuffix(b, "@helper") {
			t.Errorf("a task was credited to the function above it: %q", b)
		}
	}
}

func TestTemporalPythonWorkflowIsTheClass(t *testing.T) {
	src := `from temporalio import workflow, activity

@activity.defn
async def compose(name):
    return name

@workflow.defn(name="Greeting")
class GreetingWorkflow:
    @workflow.run
    async def run(self, name):
        return await workflow.execute_activity(compose, name)

async def main(client):
    await client.execute_workflow(GreetingWorkflow.run, "x", id="1")
    await client.start_workflow("Greeting", "y", id="2")
`
	got := bindingSet(Extract("wf.py", []byte(src)))
	for _, want := range []string{
		"temporal consume compose @compose",
		"temporal consume Greeting @GreetingWorkflow",
		"temporal publish compose @run",
		"temporal publish GreetingWorkflow @main",
		"temporal publish Greeting @main",
	} {
		if !got[want] {
			t.Errorf("missing %q; got %v", want, got)
		}
	}
	for b := range got {
		if strings.Contains(b, "consume run") {
			t.Errorf("the workflow was named after its method: %q", b)
		}
	}
}

func TestBullNeedsAnImportAndAQueue(t *testing.T) {
	notBull := `// bullet list helper
export function f(params, form) {
  params.add('page', 1);
  form.add('name');
}
`
	if got := Extract("ui.ts", []byte(notBull)); len(got) != 0 {
		t.Errorf("a file that only says \"bullet\" gave %+v", got)
	}
	src := `import { Queue, Worker } from 'bullmq';
const emails = new Queue('emails');
const seen = new Set();
export async function signup(user) {
  seen.add('x');
  await emails.add('welcome', { user });
  await this.reportQueue.add('daily', {});
}
new Worker('emails', async job => {});
`
	got := bindingSet(Extract("jobs.ts", []byte(src)))
	if !got["bullmq publish welcome @"] && !got["bullmq publish welcome @signup"] {
		t.Errorf("emails.add('welcome') not read as a job; got %v", got)
	}
	found := false
	for b := range got {
		if strings.Contains(b, "publish daily") {
			found = true
		}
		if strings.Contains(b, "publish x") {
			t.Errorf("a Set's add read as a job: %q", b)
		}
	}
	if !found {
		t.Errorf("reportQueue.add('daily') not read as a job; got %v", got)
	}
}
