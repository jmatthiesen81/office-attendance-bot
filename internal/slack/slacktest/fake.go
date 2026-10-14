// Package slacktest provides an in-memory fake of slack.API for tests.
package slacktest

import (
	"context"
	"fmt"
	"sort"
	"sync"

	goslack "github.com/slack-go/slack"

	"officebot/internal/slack"
)

// Post is a recorded chat.postMessage.
type Post struct {
	Channel string
	TS      string
	Msg     slack.Message
}

// Update is a recorded chat.update.
type Update struct {
	Channel string
	TS      string
	Msg     slack.Message
}

// View is a recorded views.open or views.push (ViewID empty, Pushed set for
// views.push) or views.update.
type View struct {
	TriggerID string
	ViewID    string
	Pushed    bool
	View      goslack.ModalViewRequest
}

// Fake records calls and lets tests inject errors.
type Fake struct {
	mu         sync.Mutex
	seq        int
	TeamID     string
	Users      map[string]*slack.User
	Members    map[string][]string
	Posts      []Post
	Updates    []Update
	Ephemerals []string
	Deleted    map[string]bool // ts of messages that "no longer exist"
	Views      []View          // views.open and views.update calls, in order

	// Fail, if set, is consulted before every call; a non-nil error is returned.
	Fail func(method, channel string) error
	// LoseNextPostResponse simulates a crash after Slack accepted a post: the
	// message is stored but an error is returned to the caller.
	LoseNextPostResponse bool
}

// New returns a fake for workspace T1.
func New() *Fake {
	return &Fake{TeamID: "T1", Users: map[string]*slack.User{}, Members: map[string][]string{}, Deleted: map[string]bool{}}
}

// AddUser registers an active human user.
func (f *Fake) AddUser(id, name string) *slack.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := &slack.User{ID: id, TeamID: f.TeamID, Name: name}
	f.Users[id] = u
	return u
}

func (f *Fake) fail(method, channel string) error {
	if f.Fail != nil {
		return f.Fail(method, channel)
	}
	return nil
}

// SlackError builds an error as returned by the Slack API.
func SlackError(code string) error { return goslack.SlackErrorResponse{Err: code} }

func (f *Fake) AuthTest(context.Context) (string, string, error) { return f.TeamID, "UBOT", nil }

func (f *Fake) ConversationMembers(_ context.Context, channelID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("conversations.members", channelID); err != nil {
		return nil, err
	}
	m, ok := f.Members[channelID]
	if !ok {
		return nil, SlackError("channel_not_found")
	}
	return m, nil
}

func (f *Fake) UserInfo(_ context.Context, userID string) (*slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("users.info", userID); err != nil {
		return nil, err
	}
	u, ok := f.Users[userID]
	if !ok {
		return nil, SlackError("user_not_found")
	}
	c := *u
	return &c, nil
}

func (f *Fake) ListUsers(context.Context) ([]slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("users.list", ""); err != nil {
		return nil, err
	}
	out := make([]slack.User, 0, len(f.Users))
	for _, u := range f.Users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *Fake) OpenDM(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("conversations.open", userID); err != nil {
		return "", err
	}
	if u, ok := f.Users[userID]; !ok || u.Deleted {
		return "", SlackError("user_not_found")
	}
	return "D" + userID, nil
}

func (f *Fake) PostMessage(_ context.Context, channelID string, m slack.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("chat.postMessage", channelID); err != nil {
		return "", err
	}
	f.seq++
	ts := fmt.Sprintf("1000.%06d", f.seq)
	f.Posts = append(f.Posts, Post{Channel: channelID, TS: ts, Msg: m})
	if f.LoseNextPostResponse {
		f.LoseNextPostResponse = false
		return "", fmt.Errorf("connection reset by peer")
	}
	return ts, nil
}

func (f *Fake) UpdateMessage(_ context.Context, channelID, ts string, m slack.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("chat.update", channelID); err != nil {
		return err
	}
	if f.Deleted[ts] {
		return SlackError("message_not_found")
	}
	f.Updates = append(f.Updates, Update{Channel: channelID, TS: ts, Msg: m})
	return nil
}

func (f *Fake) PostEphemeral(_ context.Context, channelID, userID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Ephemerals = append(f.Ephemerals, userID+": "+text)
	return nil
}

func (f *Fake) FindMessageByMetadata(_ context.Context, channelID, eventType, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("conversations.history", channelID); err != nil {
		return "", false, err
	}
	for i := len(f.Posts) - 1; i >= 0; i-- {
		p := f.Posts[i]
		if p.Channel == channelID && !f.Deleted[p.TS] && p.Msg.Metadata != nil &&
			p.Msg.Metadata.EventType == eventType && fmt.Sprint(p.Msg.Metadata.EventPayload["key"]) == key {
			return p.TS, true, nil
		}
	}
	return "", false, nil
}

func (f *Fake) Permalink(_ context.Context, channelID, ts string) (string, error) {
	return "https://example.slack.com/archives/" + channelID + "/p" + ts, nil
}

// PostsTo returns the posts to a channel.
func (f *Fake) PostsTo(channel string) []Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Post
	for _, p := range f.Posts {
		if p.Channel == channel {
			out = append(out, p)
		}
	}
	return out
}

// UpdatesTo returns the updates to a channel.
func (f *Fake) UpdatesTo(channel string) []Update {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Update
	for _, u := range f.Updates {
		if u.Channel == channel {
			out = append(out, u)
		}
	}
	return out
}

// EphemeralCount returns the number of ephemeral messages.
func (f *Fake) EphemeralCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Ephemerals)
}

func (f *Fake) OpenView(_ context.Context, triggerID string, v goslack.ModalViewRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("views.open", ""); err != nil {
		return err
	}
	f.Views = append(f.Views, View{TriggerID: triggerID, View: v})
	return nil
}

func (f *Fake) PushView(_ context.Context, triggerID string, v goslack.ModalViewRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("views.push", ""); err != nil {
		return err
	}
	f.Views = append(f.Views, View{TriggerID: triggerID, Pushed: true, View: v})
	return nil
}

func (f *Fake) UpdateView(_ context.Context, viewID string, v goslack.ModalViewRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("views.update", ""); err != nil {
		return err
	}
	f.Views = append(f.Views, View{ViewID: viewID, View: v})
	return nil
}

// LastView returns the most recently opened or updated view.
func (f *Fake) LastView() (View, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Views) == 0 {
		return View{}, false
	}
	return f.Views[len(f.Views)-1], true
}
