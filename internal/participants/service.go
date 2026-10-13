// Package participants determines who is asked in a poll.
package participants

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"officebot/internal/domain"
	"officebot/internal/slack"
)

// Source configures where participants come from: either all members of a
// channel (mode A) or an explicit list of user IDs (mode B).
type Source struct {
	ChannelID string
	UserIDs   []string
	Exclude   []string
}

// Service resolves the participant list.
type Service struct {
	api       slack.API
	src       Source
	teamID    string
	botUserID string
	log       *slog.Logger
}

// New creates a participant resolver. teamID and botUserID come from auth.test.
func New(api slack.API, src Source, teamID, botUserID string, log *slog.Logger) *Service {
	return &Service{api: api, src: src, teamID: teamID, botUserID: botUserID, log: log}
}

// Reasons why a user is not asked.
const (
	ReasonBot         = "bot"
	ReasonDeactivated = "deactivated"
	ReasonExcluded    = "excluded"
	ReasonForeignTeam = "other workspace"
)

// Exclusion returns why u would not be asked, or "" if u is eligible.
func (s *Service) Exclusion(u *slack.User) string {
	switch {
	case u.Deleted:
		return ReasonDeactivated
	case u.IsBot || u.IsAppUser || u.ID == "USLACKBOT" || u.ID == s.botUserID:
		return ReasonBot
	case s.teamID != "" && u.TeamID != "" && u.TeamID != s.teamID:
		return ReasonForeignTeam
	}
	for _, id := range s.src.Exclude {
		if id == u.ID {
			return ReasonExcluded
		}
	}
	return ""
}

// Resolve returns the active human participants, deduplicated and without
// excluded users, in a stable order.
func (s *Service) Resolve(ctx context.Context) ([]domain.Participant, error) {
	ids := s.src.UserIDs
	if s.src.ChannelID != "" {
		members, err := s.api.ConversationMembers(ctx, s.src.ChannelID)
		if err != nil {
			return nil, fmt.Errorf("list members of %s: %w", s.src.ChannelID, err)
		}
		ids = members
	}

	// Excluded users and the bot itself are skipped without an API call.
	seen := map[string]bool{s.botUserID: true}
	for _, id := range s.src.Exclude {
		seen[id] = true
	}
	var out []domain.Participant
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true

		u, err := s.api.UserInfo(ctx, id)
		if err != nil {
			if slack.IsUnreachableUser(err) {
				s.log.Warn("skipping unknown user", "user", id, "err", err)
				continue
			}
			return nil, fmt.Errorf("users.info %s: %w", id, err)
		}
		if reason := s.Exclusion(u); reason != "" {
			s.log.Debug("skipping user", "user", id, "reason", reason)
			continue
		}
		out = append(out, domain.Participant{SlackUserID: id, DisplayName: u.Name})
	}
	return out, nil
}

// ReasonNotInTarget means the user is neither in SLACK_USER_IDS nor a member
// of the participants channel.
const ReasonNotInTarget = "not in target group"

// Eligible checks whether a single user belongs to the configured target
// group right now and would be asked. It is used for late joiners. If the
// user is not eligible, the returned reason is non-empty.
func (s *Service) Eligible(ctx context.Context, userID string) (*domain.Participant, string, error) {
	inTarget := false
	if s.src.ChannelID != "" {
		members, err := s.api.ConversationMembers(ctx, s.src.ChannelID)
		if err != nil {
			return nil, "", fmt.Errorf("list members of %s: %w", s.src.ChannelID, err)
		}
		inTarget = contains(members, userID)
	} else {
		inTarget = contains(s.src.UserIDs, userID)
	}
	if !inTarget {
		return nil, ReasonNotInTarget, nil
	}
	u, err := s.api.UserInfo(ctx, userID)
	if err != nil {
		return nil, "", fmt.Errorf("users.info %s: %w", userID, err)
	}
	if reason := s.Exclusion(u); reason != "" {
		return nil, reason, nil
	}
	return &domain.Participant{SlackUserID: userID, DisplayName: u.Name}, "", nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Entry is one user in the directory listing.
type Entry struct {
	User slack.User
	// Exclusion is "" for users who would be asked, otherwise the reason.
	Exclusion string
}

// Directory lists the users of the workspace (or only the members of
// channelID, if set) sorted by name, each with the reason why they would not
// be asked. It helps to fill SLACK_USER_IDS.
func (s *Service) Directory(ctx context.Context, channelID string) ([]Entry, error) {
	users, err := s.api.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("users.list: %w", err)
	}
	var members map[string]bool
	if channelID != "" {
		ids, err := s.api.ConversationMembers(ctx, channelID)
		if err != nil {
			return nil, fmt.Errorf("list members of %s: %w", channelID, err)
		}
		members = make(map[string]bool, len(ids))
		for _, id := range ids {
			members[id] = true
		}
	}
	var out []Entry
	for i := range users {
		if members != nil && !members[users[i].ID] {
			continue
		}
		out = append(out, Entry{User: users[i], Exclusion: s.Exclusion(&users[i])})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].User.Name) < strings.ToLower(out[j].User.Name)
	})
	return out, nil
}
