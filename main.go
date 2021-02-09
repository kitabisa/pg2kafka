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

	// Process any events left in the queue
	processQueue(producer, eq)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	logrus.Info("pg2kafka is now listening to notifications")
	waitForNotification(listener, producer, eq, signals)
}

// ProcessEvents queries the database for unprocessed events and produces them
// to kafka.
func ProcessEvents(p *kafka.Producer, eq *eventqueue.Queue, tableName string) {
	events, err := eq.FetchUnprocessedRecords(tableName)
	if err != nil {
		logrus.Errorf("Error listening to pg %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(len(events))
	eventIDsChan := make(chan int, len(events))

	produceMessages(p, events, eventIDsChan, func() { wg.Done() })

	go func() {
		defer close(eventIDsChan)
		wg.Wait()
	}()

	if len(events) > 0 {
		statements, eventIDs := prepareEventIDStatements(eventIDsChan)
		err = eq.MarkEventAsProcessed(statements, eventIDs)
		if err != nil {
			logrus.Infof("Error marking record as processed %v", err)
		}
	}
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

			for i := 0; i <= pageCount; i++ {
				ProcessEvents(p, eq, r.TableName)
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

func produceMessages(p *kafka.Producer, events []*eventqueue.Event, eventIDsChan chan int, onDone func()) {
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
		if os.Getenv("DRY_RUN") != "" {
			go func() {
				defer onDone()
				logrus.Infof("id: %s, table: %s, statement: %s, data: %v", event.ExternalID, event.TableName,
					event.Statement,
					string(event.Data),
					string(event.PreviousData))
				eventIDsChan <- event.ID
			}()
		} else {
			go func() {
				defer onDone()
				for e := range p.Events() {
					switch ev := e.(type) {
					case *kafka.Message:
						m := ev
						if m.TopicPartition.Error != nil {
							fmt.Printf("Delivery failed: %v\n", m.TopicPartition.Error)
							eventIDsChan <- -1
						} else {
							fmt.Printf("Delivered message to topic %s [%d] at offset %v\n",
								*m.TopicPartition.Topic, m.TopicPartition.Partition, m.TopicPartition.Offset)
							eventIDsChan <- event.ID
						}
						return

					default:
						fmt.Printf("Ignored event: %s\n", ev)
					}
				}
			}()
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

func prepareEventIDStatements(eventIDsChan chan int) (statements string, eventIDs []interface{}) {
	i := 1
	for c := range eventIDsChan {
		if c != -1 {
			statements += fmt.Sprintf("$%d,", i)
			eventIDs = append(eventIDs, c)
			i++
		}
	}
	statements = statements[:len(statements)-1] // remove the trailing comma
	return
}
