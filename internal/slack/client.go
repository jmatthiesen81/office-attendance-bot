// Package slack wraps the Slack Web API behind a small interface, builds the
// Block Kit messages and handles Socket Mode interactions.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	goslack "github.com/slack-go/slack"
)

// Message is an outgoing Slack message.
type Message struct {
	Text     string
	Blocks   []goslack.Block
	ThreadTS string
	Metadata *goslack.SlackMetadata
}

// User is the subset of Slack user data the bot needs.
type User struct {
	ID        string
	TeamID    string
	Name      string // display name, falling back to real name and handle
	RealName  string
	Deleted   bool
	IsBot     bool
	IsAppUser bool
}

// API is the subset of the Slack Web API used by the bot. It is an interface
// so that business logic can be tested with a fake.
type API interface {
	AuthTest(ctx context.Context) (teamID, botUserID string, err error)
	ConversationMembers(ctx context.Context, channelID string) ([]string, error)
	UserInfo(ctx context.Context, userID string) (*User, error)
	// ListUsers returns all users of the workspace (users.list, paginated).
	ListUsers(ctx context.Context) ([]User, error)
	OpenDM(ctx context.Context, userID string) (channelID string, err error)
	PostMessage(ctx context.Context, channelID string, msg Message) (ts string, err error)
	UpdateMessage(ctx context.Context, channelID, ts string, msg Message) error
	PostEphemeral(ctx context.Context, channelID, userID, text string) error
	// FindMessageByMetadata searches the latest messages of a channel for a
	// bot message carrying the given metadata event type and "key" value.
	FindMessageByMetadata(ctx context.Context, channelID, eventType, key string) (ts string, found bool, err error)
	Permalink(ctx context.Context, channelID, ts string) (string, error)
	// OpenView opens a modal for the user who triggered triggerID.
	OpenView(ctx context.Context, triggerID string, view goslack.ModalViewRequest) error
	// UpdateView replaces an open modal with the given content.
	UpdateView(ctx context.Context, viewID string, view goslack.ModalViewRequest) error
}

// Metadata event types used to recognise our own messages.
const (
	MetaPollDM  = "office_poll_dm"
	MetaSummary = "office_poll_summary"
)

// NewMetadata returns message metadata identifying a poll message.
func NewMetadata(eventType, key string) *goslack.SlackMetadata {
	return &goslack.SlackMetadata{EventType: eventType, EventPayload: map[string]any{"key": key}}
}

// ------------------------------------------------------------- real client

// Client implements API with slack-go.
type Client struct {
	api *goslack.Client
}

// NewClient creates a Web API client for the bot token.
func NewClient(api *goslack.Client) *Client { return &Client{api: api} }

func (c *Client) AuthTest(ctx context.Context) (string, string, error) {
	r, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return "", "", err
	}
	return r.TeamID, r.UserID, nil
}

func (c *Client) ConversationMembers(ctx context.Context, channelID string) ([]string, error) {
	var all []string
	cursor := ""
	for {
		ids, next, err := c.api.GetUsersInConversationContext(ctx, &goslack.GetUsersInConversationParameters{ChannelID: channelID, Cursor: cursor, Limit: 200})
		if err != nil {
			return nil, err
		}
		all = append(all, ids...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

func toUser(u *goslack.User) User {
	name := u.Profile.DisplayName
	if name == "" {
		name = u.RealName
	}
	if name == "" {
		name = u.Name
	}
	return User{ID: u.ID, TeamID: u.TeamID, Name: name, RealName: u.RealName, Deleted: u.Deleted, IsBot: u.IsBot, IsAppUser: u.IsAppUser}
}

func (c *Client) UserInfo(ctx context.Context, userID string) (*User, error) {
	u, err := c.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := toUser(u)
	return &out, nil
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	// GetUsersContext follows the pagination cursor and waits on rate limits.
	users, err := c.api.GetUsersContext(ctx, goslack.GetUsersOptionLimit(200))
	if err != nil {
		return nil, err
	}
	out := make([]User, len(users))
	for i := range users {
		out[i] = toUser(&users[i])
	}
	return out, nil
}

func (c *Client) OpenDM(ctx context.Context, userID string) (string, error) {
	ch, _, _, err := c.api.OpenConversationContext(ctx, &goslack.OpenConversationParameters{Users: []string{userID}, ReturnIM: true})
	if err != nil {
		return "", err
	}
	return ch.ID, nil
}

func msgOptions(m Message) []goslack.MsgOption {
	opts := []goslack.MsgOption{goslack.MsgOptionText(m.Text, false)}
	if len(m.Blocks) > 0 {
		opts = append(opts, goslack.MsgOptionBlocks(m.Blocks...))
	}
	if m.ThreadTS != "" {
		opts = append(opts, goslack.MsgOptionTS(m.ThreadTS))
	}
	if m.Metadata != nil {
		opts = append(opts, goslack.MsgOptionMetadata(*m.Metadata))
	}
	return opts
}

func (c *Client) PostMessage(ctx context.Context, channelID string, m Message) (string, error) {
	_, ts, err := c.api.PostMessageContext(ctx, channelID, msgOptions(m)...)
	return ts, err
}

func (c *Client) UpdateMessage(ctx context.Context, channelID, ts string, m Message) error {
	_, _, _, err := c.api.UpdateMessageContext(ctx, channelID, ts, msgOptions(m)...)
	return err
}

func (c *Client) PostEphemeral(ctx context.Context, channelID, userID, text string) error {
	_, err := c.api.PostEphemeralContext(ctx, channelID, userID, goslack.MsgOptionText(text, false))
	return err
}

func (c *Client) FindMessageByMetadata(ctx context.Context, channelID, eventType, key string) (string, bool, error) {
	r, err := c.api.GetConversationHistoryContext(ctx, &goslack.GetConversationHistoryParameters{
		ChannelID: channelID, Limit: 100, IncludeAllMetadata: true,
	})
	if err != nil {
		return "", false, err
	}
	for _, m := range r.Messages {
		if m.Metadata.EventType == eventType && fmt.Sprint(m.Metadata.EventPayload["key"]) == key {
			return m.Timestamp, true, nil
		}
	}
	return "", false, nil
}

func (c *Client) Permalink(ctx context.Context, channelID, ts string) (string, error) {
	return c.api.GetPermalinkContext(ctx, &goslack.PermalinkParameters{Channel: channelID, Ts: ts})
}

func (c *Client) OpenView(ctx context.Context, triggerID string, view goslack.ModalViewRequest) error {
	_, err := c.api.OpenViewContext(ctx, triggerID, view)
	return err
}

func (c *Client) UpdateView(ctx context.Context, viewID string, view goslack.ModalViewRequest) error {
	// No hash: the view is always replaced with the current database state.
	_, err := c.api.UpdateViewContext(ctx, view, "", "", viewID)
	return err
}

// ---------------------------------------------------------- dry-run client

// DryRun implements API without contacting Slack. Every call is logged.
type DryRun struct {
	Log *slog.Logger
	seq atomic.Int64
}

func (d *DryRun) AuthTest(context.Context) (string, string, error) { return "TDRYRUN", "UDRYBOT", nil }

func (d *DryRun) ConversationMembers(_ context.Context, channelID string) ([]string, error) {
	d.Log.Info("dry-run: conversations.members returns no members", "channel", channelID)
	return nil, nil
}

func (d *DryRun) UserInfo(_ context.Context, userID string) (*User, error) {
	return &User{ID: userID, TeamID: "TDRYRUN", Name: userID}, nil
}

func (d *DryRun) ListUsers(context.Context) ([]User, error) {
	d.Log.Info("dry-run: users.list returns no users")
	return nil, nil
}

func (d *DryRun) OpenDM(_ context.Context, userID string) (string, error) { return "D" + userID, nil }

func (d *DryRun) PostMessage(_ context.Context, channelID string, m Message) (string, error) {
	ts := fmt.Sprintf("%d.%06d", time.Now().Unix(), d.seq.Add(1))
	d.Log.Info("dry-run: chat.postMessage", "channel", channelID, "ts", ts, "thread_ts", m.ThreadTS, "text", m.Text, "blocks", RenderPlain(m.Blocks))
	return ts, nil
}

func (d *DryRun) UpdateMessage(_ context.Context, channelID, ts string, m Message) error {
	d.Log.Info("dry-run: chat.update", "channel", channelID, "ts", ts, "text", m.Text, "blocks", RenderPlain(m.Blocks))
	return nil
}

func (d *DryRun) PostEphemeral(_ context.Context, channelID, userID, text string) error {
	d.Log.Info("dry-run: chat.postEphemeral", "channel", channelID, "user", userID, "text", text)
	return nil
}

func (d *DryRun) FindMessageByMetadata(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (d *DryRun) Permalink(_ context.Context, channelID, ts string) (string, error) {
	return "https://example.slack.com/archives/" + channelID + "/p" + strings.ReplaceAll(ts, ".", ""), nil
}

func (d *DryRun) OpenView(_ context.Context, triggerID string, v goslack.ModalViewRequest) error {
	d.Log.Info("dry-run: views.open", "trigger", triggerID, "blocks", RenderPlain(v.Blocks.BlockSet))
	return nil
}

func (d *DryRun) UpdateView(_ context.Context, viewID string, v goslack.ModalViewRequest) error {
	d.Log.Info("dry-run: views.update", "view", viewID, "blocks", RenderPlain(v.Blocks.BlockSet))
	return nil
}

// RenderPlain renders blocks as plain text for logs and tests.
func RenderPlain(blocks []goslack.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch b := b.(type) {
		case *goslack.HeaderBlock:
			sb.WriteString("# " + b.Text.Text + "\n")
		case *goslack.SectionBlock:
			if b.Text != nil {
				sb.WriteString(b.Text.Text)
				if b.Accessory != nil && b.Accessory.ButtonElement != nil {
					sb.WriteString(" [" + b.Accessory.ButtonElement.Text.Text + "]")
				}
				sb.WriteString("\n")
			}
		case *goslack.InputBlock:
			sb.WriteString(b.Label.Text + ": ")
			if dp, ok := b.Element.(*goslack.DatePickerBlockElement); ok {
				sb.WriteString(dp.InitialDate)
			}
			sb.WriteString("\n")
		case *goslack.ContextBlock:
			for _, e := range b.ContextElements.Elements {
				if t, ok := e.(*goslack.TextBlockObject); ok {
					sb.WriteString(t.Text + "\n")
				}
			}
		case *goslack.DividerBlock:
			sb.WriteString("---\n")
		case *goslack.ActionBlock:
			for _, e := range b.Elements.ElementSet {
				switch e := e.(type) {
				case *goslack.CheckboxGroupsBlockElement:
					selected := map[string]bool{}
					for _, o := range e.InitialOptions {
						selected[o.Value] = true
					}
					for _, o := range e.Options {
						mark := "[ ]"
						if selected[o.Value] {
							mark = "[x]"
						}
						sb.WriteString(mark + " " + o.Text.Text + "\n")
					}
				case *goslack.ButtonBlockElement:
					sb.WriteString("[" + e.Text.Text + "]\n")
				}
			}
		}
	}
	return sb.String()
}

// ------------------------------------------------------- error handling

// ErrorCode returns the Slack error code ("channel_not_found", …) or "".
func ErrorCode(err error) string {
	var se goslack.SlackErrorResponse
	if errors.As(err, &se) {
		return se.Err
	}
	if err == nil {
		return ""
	}
	// slack-go returns most API errors as plain errors carrying the code.
	msg := err.Error()
	if !strings.ContainsAny(msg, " :") {
		return msg
	}
	return ""
}

// RetryAfter returns the delay requested by a rate-limit response.
func RetryAfter(err error) (time.Duration, bool) {
	var rl *goslack.RateLimitedError
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	return 0, false
}

var permanentCodes = map[string]bool{
	"missing_scope": true, "not_authed": true, "invalid_auth": true, "account_inactive": true,
	"token_revoked": true, "no_permission": true, "not_in_channel": true, "channel_not_found": true,
	"is_archived": true, "user_not_found": true, "user_disabled": true, "cannot_dm_bot": true,
	"user_not_visible": true, "invalid_blocks": true, "msg_too_long": true, "restricted_action": true,
	"ekm_access_denied": true, "team_access_not_granted": true, "invalid_metadata_format": true,
}

// IsPermanent reports whether retrying cannot help.
func IsPermanent(err error) bool { return permanentCodes[ErrorCode(err)] }

// IsUnreachableUser reports errors meaning the recipient cannot be messaged.
func IsUnreachableUser(err error) bool {
	switch ErrorCode(err) {
	case "user_not_found", "user_disabled", "account_inactive", "cannot_dm_bot", "user_not_visible":
		return true
	}
	return false
}

// IsMessageGone reports whether a message to update no longer exists.
func IsMessageGone(err error) bool {
	switch ErrorCode(err) {
	case "message_not_found", "cant_update_message", "channel_not_found":
		return true
	}
	return false
}
