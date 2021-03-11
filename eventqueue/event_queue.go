package eventqueue

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"time"

	"github.com/pkg/errors"
)

const (
	selectExternalIDRelationsQuery = `
		SELECT id, external_id, table_name FROM pg2kafka.external_id_relations
	`

	selectUnprocessedEventsQuery = `
		SELECT id, uuid, external_id, table_name, statement, data, previous_data, created_at
		FROM pg2kafka.outbound_event_queue
		WHERE processed = false AND table_name = $1 AND ID > $2
		ORDER BY id ASC
		LIMIT 1000
	`

	markEventAsProcessedQuery = `
		UPDATE pg2kafka.outbound_event_queue
		SET processed = true
		WHERE id in (%s) AND processed = false
	`

	countUnprocessedEventsQuery = `
		SELECT count(*) AS count
		FROM pg2kafka.outbound_event_queue
		WHERE processed IS FALSE AND table_name = $1
	`
)

// ByteString is a special type of byte array with implemented interfaces to
// convert from and to JSON and SQL values.
type ByteString []byte

type ExternalIDRelation struct {
	ID int `json:"id"`
	ExternalID string `json:"external_id"`
	TableName string `json:"table_name"`
}

// Event represents the queued event in the database
type Event struct {
	ID           int             `json:"id"`
	UUID         string          `json:"uuid"`
	ExternalID   ByteString      `json:"external_id"`
	TableName    string          `json:"-"`
	Statement    string          `json:"statement"`
	Data         json.RawMessage `json:"data"`
	PreviousData json.RawMessage `json:"previous_data,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	Processed    bool            `json:"-"`
}

// Queue represents the queue of snapshot/create/update/delete events stored in
// the database.
type Queue struct {
	db *sql.DB
}

// New creates a new Queue, connected to the given database URL.
func New(conninfo string) (*Queue, error) {
	db, err := sql.Open("postgres", conninfo)
	if err != nil {
		return nil, err
	}

	return &Queue{db: db}, nil
}

// NewWithDB creates a new Queue with the given database.
func NewWithDB(db *sql.DB) *Queue {
	return &Queue{db: db}
}

// FetchUnprocessedRecords fetches a page (up to 1000) of events that have not
// been marked as processed yet.
func (eq *Queue) FetchUnprocessedRecords(tableName string, lastID int) ([]*Event, error) {
	rows, err := eq.db.Query(selectUnprocessedEventsQuery, tableName, lastID)
	if err != nil {
		return nil, err
	}

	messages := []*Event{}
	for rows.Next() {
		previousData := &json.RawMessage{}
		msg := &Event{}
		err = rows.Scan(
			&msg.ID,
			&msg.UUID,
			&msg.ExternalID,
			&msg.TableName,
			&msg.Statement,
			&msg.Data,
			&previousData,
			&msg.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		if previousData != nil {
			msg.PreviousData = *previousData
		}
		messages = append(messages, msg)
	}

	if cerr := rows.Close(); cerr != nil {
		return nil, cerr
	}
	return messages, nil
}

func (eq *Queue) FetchExternalIDRelations() ([]*ExternalIDRelation, error) {
	rows, err := eq.db.Query(selectExternalIDRelationsQuery)
	if err != nil {
		return nil, err
	}

	externalIDRelation := []*ExternalIDRelation{}
	for rows.Next() {
		eir := &ExternalIDRelation{}
		err = rows.Scan(
			&eir.ID,
			&eir.ExternalID,
			&eir.TableName,
		)
		if err != nil {
			return nil, err
		}
		externalIDRelation = append(externalIDRelation, eir)
	}

	if cerr := rows.Close(); cerr != nil {
		return nil, cerr
	}
	return externalIDRelation, nil
}

// UnprocessedEventPagesCount returns how many "pages" of events there are
// queued in the database. Currently page-size is hard-coded to 1000 events per
// page.
func (eq *Queue) UnprocessedEventPagesCount(tableName string) (int, error) {
	count := 0
	err := eq.db.QueryRow(countUnprocessedEventsQuery, tableName).Scan(&count)
	if err != nil {
		return 0, err
	}

	fmt.Printf("ini table: %s, count: %d\n", tableName, count)

	limit := 1000
	return int(math.Ceil(float64(count) / float64(limit))), nil
}

// MarkEventAsProcessed marks an even as processed.
func (eq *Queue) MarkEventAsProcessed(statements string, eventIDs []interface{}) (err error) {
	query := fmt.Sprintf(markEventAsProcessedQuery, statements)
	_, err = eq.db.Exec(query, eventIDs...)
	return
}

// Close closes the Queue's database connection.
func (eq *Queue) Close() error {
	return eq.db.Close()
}

// ConfigureOutboundEventQueueAndTriggers will set up a new schema 'pg2kafka', with
// an 'outbound_event_queue' table that is used to store events, and all the
// triggers necessary to snapshot and start tracking changes for a given table.
func (eq *Queue) ConfigureOutboundEventQueueAndTriggers(path string) error {
	migration, err := ioutil.ReadFile(path + "/migrations.sql") // nolint: gosec
	if err != nil {
		return errors.Wrap(err, "error reading migration")
	}

	_, err = eq.db.Exec(string(migration))
	if err != nil {
		return errors.Wrap(err, "failed to create table")
	}

	functions, err := ioutil.ReadFile(path + "/triggers.sql") // nolint: gosec
	if err != nil {
		return errors.Wrap(err, "Error loading functions")
	}

	_, err = eq.db.Exec(string(functions))
	if err != nil {
		return errors.Wrap(err, "Error creating triggers")
	}

	return nil
}

// MarshalJSON implements the json.Marshaler interface.
func (b *ByteString) MarshalJSON() ([]byte, error) {
	if *b == nil {
		return []byte("null"), nil
	}

	return append(append([]byte(`"`), *b...), byte('"')), nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (b *ByteString) UnmarshalJSON(d []byte) error {
	var s string
	err := json.Unmarshal(d, &s)
	*b = ByteString(s)
	return err
}

// Value implements the driver.Valuer interface.
func (b *ByteString) Value() (driver.Value, error) {
	return string(*b), nil
}

// Scan implements the sql.Scanner interface.
func (b *ByteString) Scan(val interface{}) error {
	switch v := val.(type) {
	case nil:
		*b = nil
	case string:
		*b = []byte(v)
	case []byte:
		*b = v
	default:
		return errors.New("unable to convert value to ByteString")
	}

	return nil
}

