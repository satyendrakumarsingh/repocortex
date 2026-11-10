package events

import (
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
