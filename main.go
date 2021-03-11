package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/kitabisa/pg2kafka/eventqueue"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var ( // nolint: gosec
	topicNamespace string
	version        string
)

func main() {

	conninfo := os.Getenv("DATABASE_URL")
	topicNamespace = parseTopicNamespace(os.Getenv("TOPIC_NAMESPACE"), parseDatabaseName(conninfo))

	eq, err := eventqueue.New(conninfo)
	if err != nil {
		logrus.Fatalf("Error opening db connection %v", err)
	}
	defer func() {
		if cerr := eq.Close(); cerr != nil {
			logrus.Fatalf("Error closing db connection %v", cerr)
		}
	}()

	if os.Getenv("PERFORM_MIGRATIONS") == "true" {
		if cerr := eq.ConfigureOutboundEventQueueAndTriggers("./sql"); cerr != nil {
			logrus.Fatalf("Error configuring outbound_event_queue and triggers %v", cerr)
		}
	} else {
		logrus.Info("Not performing database migrations due to missing `PERFORM_MIGRATIONS`.")
	}

	producer := setupProducer()
	defer producer.Close()
	defer producer.Flush(1000)

	reportProblem := func(ev pq.ListenerEventType, err error) {
		if err != nil {
			logrus.Errorf("Error handling postgres notify %v", err)
		}
	}
	listener := pq.NewListener(conninfo, 10*time.Second, time.Minute, reportProblem)
	if err := listener.Listen("outbound_event_queue"); err != nil {
		logrus.Errorf("Error listening to pg %v", err)
	}
	defer func() {
		if cerr := listener.Close(); cerr != nil {
			logrus.Errorf("Error closing listener %v", cerr)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	isDryRun := os.Getenv("DRY_RUN") != ""
	if !isDryRun {
		go producerCallback(eq, producer, signals)
	}

	// Process any events left in the queue
	processQueue(producer, eq)

	logrus.Info("pg2kafka is now listening to notifications")
	waitForNotification(listener, producer, eq, signals)
}

func markEventsAsProcessed(eq *eventqueue.Queue, eventIDs []interface{}) {
	statements, eventIDs := prepareEventIDStatements(eventIDs)
	err := eq.MarkEventAsProcessed(statements, eventIDs)
	if err != nil {
		logrus.Errorf("Error marking record as processed %v", err)
	}

}

func producerCallback(eq *eventqueue.Queue, producer *kafka.Producer, signals chan os.Signal) {
	var eventIDs []interface{}
	var eventQueue eventqueue.Event
	for {
		select {
		case <-signals:
			return
		case <-time.After(5 * time.Second):
			if len(eventIDs) > 0 {
				markEventsAsProcessed(eq, eventIDs)
				eventIDs = nil
			}
		case e, ok := <-producer.Events():
			if !ok {
				return
			}
			switch ev := e.(type) {
			case *kafka.Message:
				m := ev
				if m.TopicPartition.Error != nil {
					fmt.Printf("Delivery failed: %v\n", m.TopicPartition.Error)
				} else {
					fmt.Printf("Delivered message to topic %s [%d] at offset %v\n",
						*m.TopicPartition.Topic, m.TopicPartition.Partition, m.TopicPartition.Offset)

					err := json.Unmarshal(m.Value, &eventQueue)
					if err != nil {
						fmt.Printf("Unmarshal failed: %v\n", err)
					} else {
						eventIDs = append(eventIDs, eventQueue.ID)
						if len(eventIDs) >= 1000 {
							markEventsAsProcessed(eq, eventIDs)
							eventIDs = nil
						}
					}
				}
			default:
			}
		}
	}
}

// ProcessEvents queries the database for unprocessed events and produces them
// to kafka.
func ProcessEvents(p *kafka.Producer, eq *eventqueue.Queue, tableName string, lastID int) (returnLastID int) {
	events, err := eq.FetchUnprocessedRecords(tableName, lastID)
	if err != nil {
		logrus.Errorf("Error listening to pg %v", err)
	}

	produceMessages(p, events)
	if len(events) > 0 {
		returnLastID = events[len(events)-1].ID
	}
	return
}

func processQueue(p *kafka.Producer, eq *eventqueue.Queue) {
	var wg sync.WaitGroup

	externalIdRelations, err := eq.FetchExternalIDRelations()
	if err != nil {
		logrus.Fatalf("Error get external id relations %v", err)
	}

	for _, relation := range externalIdRelations {
		wg.Add(1)
		go func(r *eventqueue.ExternalIDRelation) {
			pageCount, err := eq.UnprocessedEventPagesCount(r.TableName)
			if err != nil {
				logrus.Fatalf("Error selecting count %v", err)
			}

			lastID := 0
			for i := 0; i <= pageCount; i++ {
				lastID = ProcessEvents(p, eq, r.TableName, lastID)
			}
			wg.Done()
		}(relation)
	}

	wg.Wait()
}

func waitForNotification(
	l *pq.Listener,
	p *kafka.Producer,
	eq *eventqueue.Queue,
	signals chan os.Signal,
) {
	for {
		select {
		case <-l.Notify:
			processQueue(p, eq)
		case <-time.After(90 * time.Second):
			go func() {
				err := l.Ping()
				if err != nil {
					logrus.Fatalf("Error pinging listener %v", err)
				}
			}()
		case <-signals:
			return
		}
	}
}

func produceMessages(p *kafka.Producer, events []*eventqueue.Event) {
	isDryRun := os.Getenv("DRY_RUN") != ""
	for _, event := range events {
		msg, err := json.Marshal(event)
		if err != nil {
			logrus.Fatalf("Error parsing event %v", err)
		}

		topic := topicName(event.TableName)
		message := &kafka.Message{
			TopicPartition: kafka.TopicPartition{
				Topic:     &topic,
				Partition: kafka.PartitionAny, // nolint: gotype
			},
			Value:     msg,
			Key:       event.ExternalID,
			Timestamp: event.CreatedAt,
		}
		if isDryRun {
			logrus.Infof("id: %s, table: %s, statement: %s", event.ExternalID, event.TableName, event.Statement)
		} else {
			p.ProduceChannel() <- message
		}
	}

	return
}

func setupProducer() *kafka.Producer {
	broker := os.Getenv("KAFKA_BROKER")
	if broker == "" {
		panic("missing KAFKA_BROKER environment")
	}

	username := os.Getenv("KAFKA_USERNAME")
	if username == "" {
		panic("missing KAFKA_USERNAME environment")
	}

	password := os.Getenv("KAFKA_PASSWORD")
	if password == "" {
		panic("missing KAFKA_PASSWORD environment")
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = os.Getenv("HOSTNAME")
	}

	p, err := kafka.NewProducer(&kafka.ConfigMap{
		"client.id":         hostname,
		"bootstrap.servers": broker,
		"partitioner":       "murmur2",
		"compression.codec": "snappy",
		"sasl.username":     username,
		"sasl.password":     password,
		"sasl.mechanism":    "PLAIN",
		"security.protocol": "SASL_SSL",
	})
	if err != nil {
		panic(errors.Wrap(err, "failed to setup producer"))
	}

	return p
}

func topicName(tableName string) string {
	return fmt.Sprintf("pg2kafka.%v.%v", topicNamespace, tableName)
}

func parseDatabaseName(conninfo string) string {
	dbURL, err := url.Parse(conninfo)
	if err != nil {
		logrus.Fatalf("Error parsing db connection string %v", err)
	}
	return strings.TrimPrefix(dbURL.Path, "/")
}

func parseTopicNamespace(topicNamespace string, databaseName string) string {
	s := databaseName
	if topicNamespace != "" {
		s = topicNamespace + "." + s
	}

	return s
}

func prepareEventIDStatements(ids []interface{}) (statements string, eventIDs []interface{}) {
	i := 1
	for _, eventID := range ids {
		statements += fmt.Sprintf("$%d,", i)
		eventIDs = append(eventIDs, eventID)
		i++
	}
	statements = statements[:len(statements)-1] // remove the trailing comma
	return
}
