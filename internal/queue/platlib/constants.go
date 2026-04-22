package platlib

// Notification channel + type constants the store uses.
//
// These match the shape expected by the DatabaseQueue's internal
// broadcaster: all three rsqueue-related notify types travel on the
// same channel ("messages") and the broadcaster's TypeMatcher filters
// them by the NotifyType field in the JSON envelope.
const (
	// channelMessages is the single channel for all rsqueue-bound
	// notifications (queue-ready, work-complete, chunk).
	channelMessages = "messages"

	// queueReadyChannel is the channel name our store uses when it
	// wants to signal "new work is available." Points at channelMessages
	// so the broadcaster picks it up.
	queueReadyChannel = channelMessages

	// notifyTypeQueue is the wire NotifyType value for "queue ready"
	// notifications. The broadcaster matcher registers this type to
	// route to the DatabaseQueue's internal subscribers.
	notifyTypeQueue uint8 = 1

	// notifyTypeWorkComplete is the wire NotifyType value for the
	// "addressed work is complete" notification. Emitted by the agent
	// callback that our adapter wires.
	notifyTypeWorkComplete uint8 = 2

	// notifyTypeChunk is the wire NotifyType for chunked-storage
	// notifications. The benchmark doesn't use chunked storage, but
	// the DatabaseQueue broadcaster subscribes to this channel, so we
	// register the type and leave it dormant.
	notifyTypeChunk uint8 = 3

	// benchQueueName is the rsqueue "name" used for every benchmark job.
	// The Harness Config's Queues map defines worker pools but rsqueue's
	// native concept here is just a string discriminator at the DB level.
	benchQueueName = "benchmark"
)
