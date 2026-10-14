package poll

import (
	"context"
	"errors"

	"officebot/internal/i18n"
	"officebot/internal/slack"
	"officebot/internal/storage"
)

// trFor returns the translator for texts only userID sees (ADR 0002): their
// own choice, else their Slack language (BOT_LANGUAGE_FROM_SLACK), else the
// team default. Database errors fall back to the next step, so a text is
// never lost over its language.
func (s *Service) trFor(ctx context.Context, userID string) *i18n.Translator {
	if tr, _ := s.chosenLanguage(ctx, userID); tr != nil {
		return tr
	}
	tr, _ := s.fallbackFor(ctx, userID)
	return tr
}

// TranslatorFor implements slack.InteractionHandler.
func (s *Service) TranslatorFor(ctx context.Context, userID string) *i18n.Translator {
	return s.trFor(ctx, userID)
}

// chosenLanguage returns the translator of the language userID chose, or nil.
// A stored language without catalog (e.g. after a rollback) is skipped but
// kept, so that it applies again once the catalog is back.
func (s *Service) chosenLanguage(ctx context.Context, userID string) (*i18n.Translator, string) {
	lang, err := s.store.GetUserLanguage(ctx, s.workspaceID, userID)
	if err != nil {
		s.log.Warn("load language choice", "user", userID, "err", err)
		return nil, ""
	}
	if lang == "" {
		return nil, ""
	}
	tr, err := i18n.New(lang)
	if err != nil {
		s.log.Warn("stored language not available, using the next one", "user", userID, "language", lang)
		return nil, lang
	}
	return tr, lang
}

// fallbackFor returns the translator that applies to userID without an own
// choice; fromSlack reports whether it is the person's Slack language.
func (s *Service) fallbackFor(ctx context.Context, userID string) (tr *i18n.Translator, fromSlack bool) {
	if s.cfg.LanguageFromSlack {
		locale, err := s.store.SlackLocale(ctx, s.workspaceID, userID)
		if err != nil {
			s.log.Warn("load slack locale", "user", userID, "err", err)
		} else if lang := i18n.FromLocale(locale); lang != "" {
			return i18n.MustNew(lang), true
		}
	}
	return s.tr(), false
}

// HandleOpenLanguage opens the language form for the clicking user. From the
// time-off form it is pushed on top, so that closing it returns there.
func (s *Service) HandleOpenLanguage(ctx context.Context, req slack.Interaction) {
	log := s.log.With("user", req.UserID)
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return
	}
	chosen, current := s.chosenLanguage(ctx, req.UserID)
	fallback, fromSlack := s.fallbackFor(ctx, req.UserID)
	tr := chosen
	if tr == nil {
		tr = fallback
	}
	v := slack.LanguageModal(tr, current, fallback, fromSlack)
	var err error
	if req.ViewID != "" {
		err = s.api.PushView(ctx, req.TriggerID, v)
	} else {
		err = s.api.OpenView(ctx, req.TriggerID, v)
	}
	if err != nil {
		log.Error("open language form", "err", err)
		s.RejectInteraction(ctx, req, tr.T("reply.try_again"))
	}
}

// SubmitLanguage validates and stores a submitted language form. It returns
// error messages per input block, or nil if the form may close.
func (s *Service) SubmitLanguage(ctx context.Context, req slack.Interaction) map[string]string {
	log := s.log.With("user", req.UserID, "language", req.Language)
	if req.TeamID != s.workspaceID {
		log.Warn("interaction from foreign workspace rejected", "team", req.TeamID)
		return nil
	}
	tr := s.trFor(ctx, req.UserID)
	var err error
	if req.Language == slack.LanguageDefault {
		_, err = s.store.ResetUserLanguage(ctx, s.workspaceID, req.UserID)
	} else if _, lerr := i18n.New(req.Language); lerr != nil {
		log.Warn("unknown language submitted")
		return map[string]string{slack.LanguageBlock: tr.T("reply.try_again")}
	} else {
		err = s.store.SetUserLanguage(ctx, s.workspaceID, req.UserID, req.Language, s.Now())
	}
	if err != nil {
		log.Error("store language", "err", err)
		return map[string]string{slack.LanguageBlock: tr.T("reply.try_again")}
	}
	log.Info("language changed")
	if err := s.LanguageChanged(ctx, req.UserID); err != nil {
		log.Error("apply language change", "err", err)
	}
	return nil
}

// LanguageChanged re-renders the delivered DMs of userID in all running
// polls in their current language. It only writes to the database, so the
// CLI can use it as well; the running bot does the sending.
func (s *Service) LanguageChanged(ctx context.Context, userID string) error {
	polls, err := s.store.OpenPolls(ctx)
	if err != nil {
		return err
	}
	now := s.Now()
	for _, p := range polls {
		if p.WorkspaceID != s.workspaceID || !p.IsAcceptingResponses(now) {
			continue
		}
		part, err := s.store.GetParticipant(ctx, p.ID, userID)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if part.DMMessageTS == "" {
			continue // not delivered yet; send_dm renders in the new language
		}
		if err := s.store.EnqueueJob(ctx, storage.JobUpdateDM, p.ID, part.ID, "", now); err != nil {
			return err
		}
	}
	s.outbox.Notify()
	return nil
}
