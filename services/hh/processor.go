package hh

import (
	"encoding/binary"
	"expvar"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/models"
	"github.com/influxdb/influxdb/tsdb"
)

const (
	pointsHint   = "points_hint"
	pointsWrite  = "points_write"
	bytesWrite   = "bytes_write"
	writeErr     = "write_err"
	unmarshalErr = "unmarshal_err"
	advanceErr   = "advance_err"
	currentErr   = "current_err"
)

type Processor struct {
	mu sync.RWMutex

	dir            string
	maxSize        int64
	maxAge         time.Duration
	retryRateLimit int64

	queues    map[uint64]*queue
	meta      metaStore
	writer    shardWriter
	metastore metaStore
	Logger    *log.Logger

	// Shard-level and node-level HH stats.
	shardStatMaps map[uint64]*expvar.Map
	nodeStatMaps  map[uint64]*expvar.Map
}

type ProcessorOptions struct {
	MaxSize        int64
	RetryRateLimit int64
}

func NewProcessor(dir string, writer shardWriter, metastore metaStore, options ProcessorOptions) (*Processor, error) {
	p := &Processor{
		dir:           dir,
		queues:        map[uint64]*queue{},
		writer:        writer,
		metastore:     metastore,
		Logger:        log.New(os.Stderr, "[handoff] ", log.LstdFlags),
		shardStatMaps: make(map[uint64]*expvar.Map),
		nodeStatMaps:  make(map[uint64]*expvar.Map),
	}
	p.setOptions(options)

	// Create the root directory if it doesn't already exist.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir all: %s", err)
	}

	if err := p.loadQueues(); err != nil {
		return p, err
	}
	return p, nil
}

func (p *Processor) setOptions(options ProcessorOptions) {
	p.maxSize = DefaultMaxSize
	if options.MaxSize != 0 {
		p.maxSize = options.MaxSize
	}

	p.retryRateLimit = DefaultRetryRateLimit
	if options.RetryRateLimit != 0 {
		p.retryRateLimit = options.RetryRateLimit
	}
}

func (p *Processor) loadQueues() error {
	files, err := ioutil.ReadDir(p.dir)
	if err != nil {
		return err
	}

	for _, file := range files {
		nodeID, err := strconv.ParseUint(file.Name(), 10, 64)
		if err != nil {
			return err
		}

		if _, err := p.addQueue(nodeID); err != nil {
			return err
		}
	}
	return nil
}

// addQueue adds a hinted-handoff queue for the given node. This function is not thread-safe
// and the caller must ensure this function is not called concurrently.
func (p *Processor) addQueue(nodeID uint64) (*queue, error) {
	path := filepath.Join(p.dir, strconv.FormatUint(nodeID, 10))
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}

	queue, err := newQueue(path, p.maxSize)
	if err != nil {
		return nil, err
	}
	if err := queue.Open(); err != nil {
		return nil, err
	}
	p.queues[nodeID] = queue

	// Create node stats for this queue.
	key := fmt.Sprintf("hh_processor:node:%d", nodeID)
	tags := map[string]string{"nodeID": strconv.FormatUint(nodeID, 10)}
	p.nodeStatMaps[nodeID] = influxdb.NewStatistics(key, "hh_processor", tags)
	return queue, nil
}

// WriteShard writes hinted-handoff data for the given shard and node. Since it may manipulate
// hinted-handoff queues, and be called concurrently, it takes a lock during queue access.
func (p *Processor) WriteShard(shardID, ownerID uint64, points []models.Point) error {
	p.mu.RLock()
	queue, ok := p.queues[ownerID]
	p.mu.RUnlock()
	if !ok {
		if err := func() error {
			// Check again under write-lock.
			p.mu.Lock()
			defer p.mu.Unlock()

			queue, ok = p.queues[ownerID]
			if !ok {
				var err error
				if queue, err = p.addQueue(ownerID); err != nil {
					return err
				}
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	// Update stats
	p.updateShardStats(shardID, pointsHint, int64(len(points)))
	p.nodeStatMaps[ownerID].Add(pointsHint, int64(len(points)))

	b := p.marshalWrite(shardID, points)
	return queue.Append(b)
}

func (p *Processor) marshalWrite(shardID uint64, points []models.Point) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, shardID)
	for _, p := range points {
		b = append(b, []byte(p.String())...)
		b = append(b, '\n')
	}
	return b
}

func (p *Processor) unmarshalWrite(b []byte) (uint64, []models.Point, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("too short: len = %d", len(b))
	}
	ownerID := binary.BigEndian.Uint64(b[:8])
	points, err := models.ParsePoints(b[8:])
	return ownerID, points, err
}

func (p *Processor) updateShardStats(shardID uint64, stat string, inc int64) {
	m, ok := p.shardStatMaps[shardID]
	if !ok {
		key := fmt.Sprintf("hh_processor:shard:%d", shardID)
		tags := map[string]string{"shardID": strconv.FormatUint(shardID, 10)}
		p.shardStatMaps[shardID] = influxdb.NewStatistics(key, "hh_processor", tags)
		m = p.shardStatMaps[shardID]
	}
	m.Add(stat, inc)
}
