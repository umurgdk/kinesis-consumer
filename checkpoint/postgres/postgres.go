package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
	// this is the postgres package so it makes sense to be here
	_ "github.com/lib/pq"
)

var getCheckpointQuery = `SELECT sequence_number 
						  FROM %s
						  WHERE namespace=$1 AND shard_id=$2`

var upsertCheckpoint = `INSERT INTO %s (namespace, shard_id, sequence_number)
						VALUES($1, $2, $3)
						ON CONFLICT (namespace, shard_id)
						DO 
						UPDATE 
						SET sequence_number= $3`

type key struct {
	streamName string
	shardID    string
}

// Option is used to override defaults when creating a new Checkpoint
type Option func(*Checkpoint)

// Checkpoint stores and retreives the last evaluated key from a DDB scan
type Checkpoint struct {
	appName     string
	conn        *sql.DB
	mu          *sync.Mutex // protects the checkpoints
	done        chan struct{}
	checkpoints map[key]string
	maxInterval time.Duration
}

// New returns a checkpoint that uses PostgresDB for underlying storage
// Using connectionStr turn it more flexible to use specific db configs
func New(appName, tableName, connectionStr string, opts ...Option) (*Checkpoint, error) {

	if tableName == "" {
		return nil, errors.New("Table name not defined")
	}

	conn, err := sql.Open("postgres", connectionStr)

	if err != nil {
		return nil, err
	}

	getCheckpointQuery = fmt.Sprintf(getCheckpointQuery, tableName)
	upsertCheckpoint = fmt.Sprintf(upsertCheckpoint, tableName)

	ck := &Checkpoint{
		conn:        conn,
		appName:     appName,
		done:        make(chan struct{}),
		maxInterval: time.Duration(1 * time.Minute),
		mu:          new(sync.Mutex),
		checkpoints: map[key]string{},
	}

	for _, opt := range opts {
		opt(ck)
	}

	go ck.loop()

	return ck, nil
}

// Get determines if a checkpoint for a particular Shard exists.
// Typically used to determine whether we should start processing the shard with
// TRIM_HORIZON or AFTER_SEQUENCE_NUMBER (if checkpoint exists).
func (c *Checkpoint) Get(streamName, shardID string) (string, error) {
	namespace := fmt.Sprintf("%s-%s", c.appName, streamName)

	var sequenceNumber string

	err := c.conn.QueryRow(getCheckpointQuery, namespace, shardID).Scan(&sequenceNumber)

	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}

		return "", err
	}

	return sequenceNumber, nil
}

// Set stores a checkpoint for a shard (e.g. sequence number of last record processed by application).
// Upon failover, record processing is resumed from this point.
func (c *Checkpoint) Set(streamName, shardID, sequenceNumber string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if sequenceNumber == "" {
		return fmt.Errorf("sequence number should not be empty")
	}

	key := key{
		streamName: streamName,
		shardID:    shardID,
	}

	c.checkpoints[key] = sequenceNumber

	return nil
}

// Shutdown the checkpoint. Save any in-flight data.
func (c *Checkpoint) Shutdown() error {
	defer c.conn.Close()

	c.done <- struct{}{}

	return c.save()
}

func (c *Checkpoint) loop() {
	tick := time.NewTicker(c.maxInterval)
	defer tick.Stop()
	defer close(c.done)

	for {
		select {
		case <-tick.C:
			c.save()
		case <-c.done:
			return
		}
	}
}

func (c *Checkpoint) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, sequenceNumber := range c.checkpoints {

		if _, err := c.conn.Exec(upsertCheckpoint, fmt.Sprintf("%s-%s", c.appName, key.streamName), key.shardID, sequenceNumber); err != nil {
			return err
		}
	}

	return nil
}
