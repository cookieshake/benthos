package service

import (
	"context"
	"errors"

	"github.com/Jeffail/benthos/v3/internal/component/buffer"
	"github.com/Jeffail/benthos/v3/internal/shutdown"
	"github.com/Jeffail/benthos/v3/lib/message"
	"github.com/Jeffail/benthos/v3/lib/types"
)

// BatchBuffer is an interface implemented by Buffers able to read and write
// message batches. Buffers are a component type that are placed after inputs,
// and decouples the acknowledgement system of the inputs from the rest of the
// pipeline.
//
// Buffers are useful when implementing buffers intended to relieve back
// pressure from upstream components, or when implementing message aggregators
// where the concept of discrete messages running through a pipeline no longer
// applies (such as with windowing algorithms).
//
// Buffers are advanced component types that weaken delivery guarantees of a
// Benthos pipeline. Therefore, if you aren't absolutely sure that a component
// you wish to build should be a buffer type then it likely shouldn't be.
type BatchBuffer interface {
	// Write a batch of messages to the buffer. If a nil error is returned the
	// upstream components will acknowledge receipt of the message, therefore
	// decoupling the input from the buffer and components downstream.
	WriteBatch(context.Context, MessageBatch) error

	// Read a batch of messages from the buffer. This call should block until
	// either a batch is ready to consume, the provided context is cancelled or
	// EndOfInput has been called which indicates that messages are no longer
	// being consumed.
	//
	// The returned acknowledge function will be called when a consumed message
	// batch has been processed and sent downstream. It is up to the buffer
	// implementation whether the ack function is used, it might be used in
	// order to "commit" the removal of a message from the buffer in cases where
	// the buffer is a persisted storage solution, or in cases where the output
	// of the buffer is temporal (a windowing algorithm, etc) it might be
	// considered correct to simply drop message batches that are not acked.
	//
	// When the buffer is closed (EndOfInput has been called and no more
	// messages are available) this method should return an ErrEndOfBuffer in
	// order to indicate the end of the buffered stream.
	//
	// It is valid to return a batch of only one message.
	ReadBatch(context.Context) (MessageBatch, AckFunc, error)

	// EndOfInput indicates to the buffer that the input has ended and that once
	// the buffer is depleted it should return ErrEndOfBuffer from ReadBatch in
	// order to gracefully shut down the pipeline.
	//
	// EndOfInput should be idempotent as it may be called more than once.
	EndOfInput()

	Closer
}

//------------------------------------------------------------------------------

// Implements buffer.ReaderWriter
type airGapBatchBuffer struct {
	b   BatchBuffer
	sig *shutdown.Signaller
}

func newAirGapBatchBuffer(b BatchBuffer) buffer.ReaderWriter {
	return &airGapBatchBuffer{b, shutdown.NewSignaller()}
}

func (a *airGapBatchBuffer) Write(ctx context.Context, msg types.Message) error {
	parts := make([]*Message, msg.Len())
	_ = msg.Iter(func(i int, part types.Part) error {
		// Copy because we ack the message after returning, therefore we lose
		// ownership of the underlying.
		parts[i] = newMessageFromPart(part).Copy()
		return nil
	})
	return a.b.WriteBatch(ctx, parts)
}

func (a *airGapBatchBuffer) Read(ctx context.Context) (types.Message, buffer.AckFunc, error) {
	batch, ackFn, err := a.b.ReadBatch(ctx)
	if err != nil {
		if errors.Is(err, ErrEndOfBuffer) {
			err = types.ErrTypeClosed
		}
		return nil, nil, err
	}
	tMsg := message.New(nil)
	for _, msg := range batch {
		tMsg.Append(msg.part)
	}
	return tMsg, func(c context.Context, aerr error) error {
		return ackFn(c, aerr)
	}, nil
}

func (a *airGapBatchBuffer) EndOfInput() {
	a.b.EndOfInput()
}

func (a *airGapBatchBuffer) Close(ctx context.Context) error {
	return a.b.Close(ctx)
}
