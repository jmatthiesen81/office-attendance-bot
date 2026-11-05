package web

import (
	"errors"
	"net/http"
	"strings"

	"officebot/internal/storage"
)

type accessData struct {
	Users           []storage.WebUser
	Me              string
	Candidates      []candidate
	DirectoryFailed bool
}

type candidate struct {
	ID, Name string
}

func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := s.Store.ListWebUsers(ctx, s.TeamID)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
		return
	}
	d := accessData{Users: users, Me: viewerFrom(r).User.SlackUserID}
	has := map[string]bool{}
	for _, u := range users {
		has[u.SlackUserID] = true
	}
	entries, err := s.People.Directory(ctx, "")
	if err != nil {
		s.Log.Warn("load slack directory", "err", err)
		d.DirectoryFailed = true
	}
	for _, e := range entries {
		if has[e.User.ID] || e.User.Deleted || e.User.IsBot || e.User.IsAppUser || (e.User.TeamID != "" && e.User.TeamID != s.TeamID) {
			continue
		}
		d.Candidates = append(d.Candidates, candidate{ID: e.User.ID, Name: e.User.Name})
	}
	s.render(w, r, http.StatusOK, "access", &Page{Title: tr(s.lang(r), "access.title"), Nav: "access", Data: d})
}

func (s *Server) handleAccessAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := strings.TrimSpace(r.PostFormValue("user"))
	if user == "" {
		user = strings.TrimSpace(r.PostFormValue("user_id"))
	}
	role := r.PostFormValue("role")
	if user == "" || !storage.ValidRole(role) {
		redirect(w, r, "/access", "", "invalid")
		return
	}
	// The name comes from Slack; this also checks that the ID exists.
	info, err := s.Slack.UserInfo(ctx, user)
	if err != nil {
		s.Log.Warn("look up slack user", "user", user, "err", err)
		redirect(w, r, "/access", "", "invalid")
		return
	}
	if info.TeamID != "" && info.TeamID != s.TeamID {
		redirect(w, r, "/access", "", "foreign_workspace")
		return
	}
	err = s.Store.AddWebUser(ctx, s.TeamID, storage.WebUser{SlackUserID: user, Role: role, DisplayName: info.Name}, viewerFrom(r).User.SlackUserID, s.Now())
	switch {
	case errors.Is(err, storage.ErrExists):
		redirect(w, r, "/access", "", "exists")
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
	default:
		redirect(w, r, "/access", "added", "")
	}
}

func (s *Server) handleAccessRole(w http.ResponseWriter, r *http.Request) {
	role := r.PostFormValue("role")
	if !storage.ValidRole(role) {
		redirect(w, r, "/access", "", "invalid")
		return
	}
	changed, err := s.Store.SetWebUserRole(r.Context(), s.TeamID, r.PostFormValue("user"), role, viewerFrom(r).User.SlackUserID, s.Now())
	switch {
	case errors.Is(err, storage.ErrLastAdmin):
		redirect(w, r, "/access", "", "last_admin")
	case errors.Is(err, storage.ErrNotFound):
		redirect(w, r, "/access", "", "not_found")
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
	case !changed:
		redirect(w, r, "/access", "unchanged", "")
	default:
		redirect(w, r, "/access", "saved", "")
	}
}

func (s *Server) handleAccessRemove(w http.ResponseWriter, r *http.Request) {
	err := s.Store.RemoveWebUser(r.Context(), s.TeamID, r.PostFormValue("user"), viewerFrom(r).User.SlackUserID, s.Now())
	switch {
	case errors.Is(err, storage.ErrLastAdmin):
		redirect(w, r, "/access", "", "last_admin")
	case errors.Is(err, storage.ErrNotFound):
		redirect(w, r, "/access", "", "not_found")
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, "err.internal", err)
	default:
		redirect(w, r, "/access", "removed", "")
	}
}
