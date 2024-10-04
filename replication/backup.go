package replication

import (
	"context"
	"io"
	"log"
	"os"
	"path"
	"sync"
	"time"

	. "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/pingcap/errors"
)

// StartBackup: Like mysqlbinlog remote raw backup
// Backup remote binlog from position (filename, offset) and write in backupDir
func (b *BinlogSyncer) StartBackup(backupDir string, p Position, timeout time.Duration) error {
	err := os.MkdirAll(backupDir, 0755)
	if err != nil {
		return errors.Trace(err)
	}
	return b.StartBackupWithHandler(p, timeout, func(filename string) (io.WriteCloser, error) {
		return os.OpenFile(path.Join(backupDir, filename), os.O_CREATE|os.O_WRONLY, 0644)
	})
}

// StartBackupWithHandler starts the backup process for the binary log using the specified position and handler.
// The process will continue until the timeout is reached or an error occurs.
//
// Parameters:
//   - p: The starting position in the binlog from which to begin the backup.
//   - timeout: The maximum duration to wait for new binlog events before stopping the backup process.
//     If set to 0, a default very long timeout (30 days) is used instead.
//   - handler: A function that takes a binlog filename and returns an WriteCloser for writing raw events to.
func (b *BinlogSyncer) StartBackupWithHandler(p Position, timeout time.Duration,
	handler func(binlogFilename string) (io.WriteCloser, error)) (retErr error) {
	if timeout == 0 {
		// a very long timeout here
		timeout = 30 * 3600 * 24 * time.Second
	}

	// Force use raw mode
	b.parser.SetRawMode(true)

	// Set up the backup event handler
	backupHandler := &BackupEventHandler{
		handler: handler,
	}

	if b.cfg.SyncMode == SyncModeSync {
		// Set the event handler in BinlogSyncer for synchronous mode
		b.SetEventHandler(backupHandler)
	}

	s, err := b.StartSync(p)
	if err != nil {
		return errors.Trace(err)
	}

	defer func() {
		b.SetEventHandler(nil) // Reset the event handler
		if backupHandler.w != nil {
			closeErr := backupHandler.w.Close()
			if retErr == nil {
				retErr = closeErr
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if b.cfg.SyncMode == SyncModeSync {
		// Synchronous mode: wait for completion or error
		select {
		case <-ctx.Done():
			return nil
		case <-b.ctx.Done():
			return nil
		case err := <-s.ech:
			return errors.Trace(err)
		}
	} else {
		// Asynchronous mode: consume events from the streamer
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-b.ctx.Done():
				return nil
			case err := <-s.ech:
				return errors.Trace(err)
			case e := <-s.ch:
				err = backupHandler.HandleEvent(e)
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
}

// BackupEventHandler handles writing events for backup
type BackupEventHandler struct {
	handler     func(binlogFilename string) (io.WriteCloser, error)
	w           io.WriteCloser
	mutex       sync.Mutex
	fsyncedChan chan struct{}
	eventCount  int // eventCount used for testing
}

func (h *BackupEventHandler) HandleEvent(e *BinlogEvent) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	var err error

	// Handle rotation events to switch binlog files
	if rotateEvent, ok := e.Event.(*RotateEvent); ok {
		if h.w != nil {
			if err := h.w.Close(); err != nil {
				return errors.Trace(err)
			}
		}

		h.w, err = h.handler(string(rotateEvent.NextLogName))
		if err != nil {
			return errors.Trace(err)
		}

		// Write the original binary binlog header
		_, err = h.w.Write(BinLogFileHeader)
		if err != nil {
			return errors.Trace(err)
		}
	}

	// Write raw event data to the current WriteCloser
	if h.w != nil {
		n, err := h.w.Write(e.RawData)
		if err != nil {
			return errors.Trace(err)
		}
		if n != len(e.RawData) {
			return errors.Trace(io.ErrShortWrite)
		}

		// Perform Sync if the writer supports it
		if f, ok := h.w.(*os.File); ok {
			if err := f.Sync(); err != nil {
				return errors.Trace(err)
			}
			// Signal that fsync has completed
			if h.fsyncedChan != nil {
				h.fsyncedChan <- struct{}{}
			}
		}
	}

	h.eventCount++

	return nil
}
