package buffer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Jeffail/benthos/v3/internal/shutdown"
	"github.com/Jeffail/benthos/v3/lib/buffer"
	"github.com/Jeffail/benthos/v3/lib/log"
	"github.com/Jeffail/benthos/v3/lib/message/tracing"
	"github.com/Jeffail/benthos/v3/lib/metrics"
	"github.com/Jeffail/benthos/v3/lib/response"
	"github.com/Jeffail/benthos/v3/lib/types"
	"github.com/Jeffail/benthos/v3/lib/util/throttle"
)

// AckFunc is a function used to acknowledge receipt of a message batch from a
// buffer. The provided error indicates whether the message batch was
// successfully delivered. Returns an error if the acknowledge was not
// propagated.
type AckFunc func(context.Context, error) error

// ReaderWriter is a read/write interface implemented by buffers.
type ReaderWriter interface {
	// Read the next oldest message batch. If the buffer has a persisted store
	// the message is preserved until the returned AckFunc is called. Some
	// temporal buffer implementations such as windowers will ignore the ack
	// func.
	Read(context.Context) (types.Message, AckFunc, error)

	// Write a new message batch to the stack.
	Write(context.Context, types.Message) error

	// EndOfInput indicates to the buffer that the input has ended and that once
	// the buffer is depleted it should return types.ErrTypeClosed from Read in
	// order to gracefully shut down the pipeline.
	//
	// EndOfInput should be idempotent as it may be called more than once.
	EndOfInput()

	// Close the buffer and all resources it has, messages should no longer be
	// written or read by the implementation and it should clean up all
	// resources.
	Close(context.Context) error
}

// Stream wraps a read/write buffer implementation with a channel based
// streaming component that satisfies the internal Benthos Consumer and Producer
// interfaces.
type Stream struct {
	stats   metrics.Type
	log     log.Modular
	typeStr string

	buffer ReaderWriter

	errThrottle *throttle.Type
	shutSig     *shutdown.Signaller

	messagesIn  <-chan types.Transaction
	messagesOut chan types.Transaction

	closedWG sync.WaitGroup
}

// NewStream creates a new Producer/Consumer around a buffer.
func NewStream(typeStr string, buffer ReaderWriter, log log.Modular, stats metrics.Type) buffer.Type {
	m := Stream{
		typeStr:     typeStr,
		stats:       stats,
		log:         log,
		buffer:      buffer,
		shutSig:     shutdown.NewSignaller(),
		messagesOut: make(chan types.Transaction),
	}
	m.errThrottle = throttle.New(throttle.OptCloseChan(m.shutSig.CloseAtLeisureChan()))
	return &m
}

//------------------------------------------------------------------------------

// inputLoop is an internal loop that brokers incoming messages to the buffer.
func (m *Stream) inputLoop() {
	defer func() {
		m.buffer.EndOfInput()
		m.closedWG.Done()
	}()

	var (
		mWriteCount = m.stats.GetCounter("write.count")
		mWriteErr   = m.stats.GetCounter("write.error")
	)

	closeAtLeisureCtx, done := m.shutSig.CloseAtLeisureCtx(context.Background())
	defer done()

	for {
		var tr types.Transaction
		var open bool
		select {
		case tr, open = <-m.messagesIn:
			if !open {
				return
			}
		case <-m.shutSig.CloseAtLeisureChan():
			return
		}

		err := m.buffer.Write(closeAtLeisureCtx, tracing.WithSiblingSpans(m.typeStr, tr.Payload))
		if err == nil {
			mWriteCount.Incr(1)
		} else {
			mWriteErr.Incr(1)
		}
		select {
		case tr.ResponseChan <- response.NewError(err):
		case <-m.shutSig.CloseNowChan():
			return
		}
	}
}

// outputLoop is an internal loop brokers buffer messages to output pipe.
func (m *Stream) outputLoop() {
	defer func() {
		_ = m.buffer.Close(context.Background())
		close(m.messagesOut)
		m.closedWG.Done()
	}()

	var (
		mReadCount   = m.stats.GetCounter("read.count")
		mReadErr     = m.stats.GetCounter("read.error")
		mSendSuccess = m.stats.GetCounter("send.success")
		mSendErr     = m.stats.GetCounter("send.error")
		mAckErr      = m.stats.GetCounter("ack.error")
		mLatency     = m.stats.GetTimer("latency")
	)

	closeNowCtx, done := m.shutSig.CloseNowCtx(context.Background())
	defer done()

	for {
		msg, ackFunc, err := m.buffer.Read(closeNowCtx)
		if err != nil {
			if err != types.ErrTypeClosed && !errors.Is(err, context.Canceled) {
				mReadErr.Incr(1)
				m.log.Errorf("Failed to read buffer: %v\n", err)
				if !m.errThrottle.Retry() {
					return
				}
			} else {
				// If our buffer is closed then we exit.
				return
			}
			continue
		}

		// It's possible that the buffer wiped our previous root span.
		tracing.InitSpans(m.typeStr, msg)

		mReadCount.Incr(1)
		m.errThrottle.Reset()

		resChan := make(chan types.Response, 1)
		select {
		case m.messagesOut <- types.NewTransaction(msg, resChan):
		case <-m.shutSig.CloseNowChan():
			return
		}

		select {
		case res, open := <-resChan:
			if !open {
				return
			}
			tracing.FinishSpans(msg)
			if res.Error() != nil {
				mSendSuccess.Incr(1)
				mLatency.Timing(time.Since(msg.CreatedAt()).Nanoseconds())
			} else {
				mSendErr.Incr(1)
			}
			if ackErr := ackFunc(closeNowCtx, res.Error()); ackErr != nil {
				mAckErr.Incr(1)
				if ackErr != types.ErrTypeClosed {
					m.log.Errorf("Failed to ack buffer message: %v\n", ackErr)
				}
			}
		case <-m.shutSig.CloseNowChan():
			return
		}
	}
}

// Consume assigns a messages channel for the output to read.
func (m *Stream) Consume(msgs <-chan types.Transaction) error {
	if m.messagesIn != nil {
		return types.ErrAlreadyStarted
	}
	m.messagesIn = msgs

	m.closedWG.Add(2)
	go m.inputLoop()
	go m.outputLoop()
	go func() {
		m.closedWG.Wait()
		m.shutSig.ShutdownComplete()
	}()
	return nil
}

// TransactionChan returns the channel used for consuming messages from this
// buffer.
func (m *Stream) TransactionChan() <-chan types.Transaction {
	return m.messagesOut
}

// CloseAsync shuts down the Stream and stops processing messages.
func (m *Stream) CloseAsync() {
	m.shutSig.CloseNow()
}

// StopConsuming instructs the buffer to stop consuming messages and close once
// the buffer is empty.
func (m *Stream) StopConsuming() {
	m.shutSig.CloseAtLeisure()
}

// WaitForClose blocks until the Stream output has closed down.
func (m *Stream) WaitForClose(timeout time.Duration) error {
	select {
	case <-m.shutSig.HasClosedChan():
	case <-time.After(timeout):
		return types.ErrTimeout
	}
	return nil
}
