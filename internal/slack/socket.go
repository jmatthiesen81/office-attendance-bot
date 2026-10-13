package slack

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	goslack "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"officebot/internal/i18n"
)

// InteractionHandler processes button clicks. It is implemented by the poll service.
type InteractionHandler interface {
	HandleSave(ctx context.Context, req Interaction)
	// HandleRequestDM (re)sends the poll DM to the user who clicked the
	// button in the channel summary.
	HandleRequestDM(ctx context.Context, req Interaction)
	// RejectInteraction informs the user that an interaction could not be processed.
	RejectInteraction(ctx context.Context, req Interaction, reason string)
	// HandleAway marks the user as away for the polled week, or undoes it.
	HandleAway(ctx context.Context, req Interaction)
	// HandleOpenTimeOff opens the time-off form.
	HandleOpenTimeOff(ctx context.Context, req Interaction)
	// HandleDeleteTimeOff deletes an entry from the open time-off form.
	HandleDeleteTimeOff(ctx context.Context, req Interaction)
	// SubmitTimeOff stores a submitted time-off form. It returns error
	// messages per input block, or nil if the form may close.
	SubmitTimeOff(ctx context.Context, req Interaction) map[string]string
}

// submitTimeout keeps form validation within Slack's 3 s acknowledgement limit.
const submitTimeout = 2500 * time.Millisecond

// Socket receives interactions via Socket Mode.
type Socket struct {
	client    *socketmode.Client
	handler   InteractionHandler
	tr        *i18n.Translator
	log       *slog.Logger
	connected atomic.Bool
	seen      *recentSet
	wg        sync.WaitGroup
}

// NewSocket creates a Socket Mode listener. api must be created with the app-level token.
func NewSocket(api *goslack.Client, handler InteractionHandler, tr *i18n.Translator, log *slog.Logger) *Socket {
	return &Socket{
		client:  socketmode.New(api),
		handler: handler,
		tr:      tr,
		log:     log,
		seen:    newRecentSet(512),
	}
}

// Connected reports whether the Socket Mode connection is currently up.
func (s *Socket) Connected() bool { return s.connected.Load() }

// Run processes events until ctx is cancelled. Reconnects are handled by
// slack-go; Run only returns early on unrecoverable errors such as invalid auth.
func (s *Socket) Run(ctx context.Context) error {
	runErr := make(chan error, 1)
	go func() { runErr <- s.client.RunContext(ctx) }()
	defer func() {
		s.connected.Store(false)
		s.wg.Wait() // let in-flight interactions finish
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-runErr:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case evt := <-s.client.Events:
			if err := s.handle(ctx, evt); err != nil {
				return err
			}
		}
	}
}

func (s *Socket) handle(ctx context.Context, evt socketmode.Event) error {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		s.log.Info("socket mode connecting")
	case socketmode.EventTypeConnected:
		s.connected.Store(true)
		s.log.Info("socket mode connected")
	case socketmode.EventTypeConnectionError, socketmode.EventTypeDisconnect:
		s.connected.Store(false)
		s.log.Warn("socket mode disconnected, reconnecting", "type", evt.Type)
	case socketmode.EventTypeInvalidAuth:
		s.connected.Store(false)
		return errors.New("socket mode: invalid app token (SLACK_APP_TOKEN)")
	case socketmode.EventTypeInteractive:
		if evt.Request == nil {
			return nil
		}
		if cb, ok := evt.Data.(goslack.InteractionCallback); ok && cb.Type == goslack.InteractionTypeViewSubmission {
			// A form submission is answered in the acknowledgement itself, so
			// that validation errors appear in the still open form. Storing is
			// idempotent, a redelivered envelope does no harm.
			s.client.Ack(*evt.Request, s.submit(&cb))
			return nil
		}
		// Acknowledge immediately (Slack requires < 3 s); work happens async.
		s.client.Ack(*evt.Request)
		if !s.seen.add(evt.Request.EnvelopeID) {
			s.log.Debug("duplicate interaction envelope ignored", "envelope", evt.Request.EnvelopeID)
			return nil
		}
		cb, ok := evt.Data.(goslack.InteractionCallback)
		if !ok {
			return nil
		}
		s.dispatch(&cb)
	case socketmode.EventTypeEventsAPI, socketmode.EventTypeSlashCommand:
		if evt.Request != nil {
			s.client.Ack(*evt.Request)
		}
	}
	return nil
}

func (s *Socket) dispatch(cb *goslack.InteractionCallback) {
	if req, ok, err := ParseSave(cb); ok {
		s.async(func(ctx context.Context) {
			if err != nil {
				s.log.Warn("invalid save interaction", "user", req.UserID, "poll", req.PollID, "err", err)
				s.handler.RejectInteraction(ctx, req, s.tr.T("reply.unreadable"))
				return
			}
			s.handler.HandleSave(ctx, req)
		})
		return
	}
	if req, ok, err := ParseRequestDM(cb); ok {
		s.async(func(ctx context.Context) {
			if err != nil {
				s.log.Warn("invalid request-DM interaction", "user", req.UserID, "poll", req.PollID, "err", err)
				return
			}
			s.handler.HandleRequestDM(ctx, req)
		})
		return
	}
	if req, ok, err := ParseAway(cb); ok {
		s.async(func(ctx context.Context) {
			if err != nil {
				s.log.Warn("invalid away interaction", "user", req.UserID, "poll", req.PollID, "err", err)
				return
			}
			s.handler.HandleAway(ctx, req)
		})
		return
	}
	if req, ok, err := ParseOpenTimeOff(cb); ok {
		s.async(func(ctx context.Context) {
			if err != nil {
				s.log.Warn("invalid time-off interaction", "user", req.UserID, "err", err)
				return
			}
			s.handler.HandleOpenTimeOff(ctx, req)
		})
		return
	}
	if req, ok, err := ParseDeleteTimeOff(cb); ok {
		s.async(func(ctx context.Context) {
			if err != nil {
				s.log.Warn("invalid time-off delete", "user", req.UserID, "err", err)
				return
			}
			s.handler.HandleDeleteTimeOff(ctx, req)
		})
		return
	}
	// Anything else (checkbox ticks etc.) is only acknowledged: nothing is
	// persisted before "Speichern".
}

// submit handles a form submission and returns the acknowledgement payload:
// nil closes the form, an errors response keeps it open.
func (s *Socket) submit(cb *goslack.InteractionCallback) any {
	req, ok, err := ParseTimeOffSubmit(cb)
	if !ok {
		return nil
	}
	if err != nil {
		s.log.Warn("invalid time-off submission", "user", req.UserID, "err", err)
		return goslack.NewErrorsViewSubmissionResponse(map[string]string{TimeOffEndBlock: s.tr.T("timeoff.error.failed")})
	}
	ctx, cancel := context.WithTimeout(context.Background(), submitTimeout)
	defer cancel()
	if errs := s.handler.SubmitTimeOff(ctx, req); len(errs) > 0 {
		return goslack.NewErrorsViewSubmissionResponse(errs)
	}
	return nil
}

// async runs fn in the background. The context is detached from the run
// context so that a shutdown lets in-flight interactions finish.
func (s *Socket) async(fn func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fn(ctx)
	}()
}

// recentSet remembers the last n keys to drop redelivered envelopes.
type recentSet struct {
	mu   sync.Mutex
	keys map[string]struct{}
	ring []string
	pos  int
}

func newRecentSet(n int) *recentSet {
	return &recentSet{keys: make(map[string]struct{}, n), ring: make([]string, n)}
}

// add returns false if key was seen recently.
func (r *recentSet) add(key string) bool {
	if key == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.keys[key]; dup {
		return false
	}
	if old := r.ring[r.pos]; old != "" {
		delete(r.keys, old)
	}
	r.ring[r.pos] = key
	r.keys[key] = struct{}{}
	r.pos = (r.pos + 1) % len(r.ring)
	return true
}
