package server

import (
	"net/http"
	"strconv"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// home редиректит на список org.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/orgs", http.StatusSeeOther)
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.q.ListOrganizationsForUser(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.Orgs(orgs))
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	o, err := s.org.CreateOrg(r.Context(), auth.UserID(r.Context()), name)
	if err != nil {
		http.Error(w, "не удалось создать организацию: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
}

func (s *Server) orgDashboard(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	projects, err := s.q.ListProjects(r.Context(), o.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cards := make([]templates.ProjectCard, 0, len(projects))
	for _, p := range projects {
		n, _ := s.q.CountEnvironments(r.Context(), p.ID)
		cards = append(cards, templates.ProjectCard{Project: p, EnvCount: n})
	}
	render(w, r, http.StatusOK, templates.OrgDashboard(o, role, cards))
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	members, err := s.q.ListMembers(r.Context(), o.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	temp := s.takeFlashPw(w, r)
	render(w, r, http.StatusOK, templates.Members(o, role, members, temp))
}

func (s *Server) createMember(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	email := r.FormValue("email")
	role := r.FormValue("role")
	if role != "admin" && role != "member" {
		role = "member"
	}
	// создать пользователя с временным паролем (или найти существующего)
	tempPw, err := auth.NewToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tempPw = tempPw[:12]
	u, err := s.q.GetUserByEmail(r.Context(), email)
	if err != nil {
		hash, herr := auth.HashPassword(tempPw)
		if herr != nil {
			http.Error(w, herr.Error(), http.StatusInternalServerError)
			return
		}
		u, err = s.q.CreateUser(r.Context(), db.CreateUserParams{Email: email, PasswordHash: hash})
		if err != nil {
			http.Error(w, "не удалось создать пользователя: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		tempPw = "" // существующий юзер — пароль не показываем
	}
	if _, err := s.q.CreateMember(r.Context(), db.CreateMemberParams{
		OrganizationID: o.ID, UserID: u.ID, Role: role,
	}); err != nil {
		http.Error(w, "пользователь уже в организации?", http.StatusBadRequest)
		return
	}
	if tempPw != "" {
		s.setFlashPw(w, tempPw)
	}
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members", http.StatusSeeOther)
}

func (s *Server) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	o, actorRole, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	mID, ok := pathID(r, "mID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	m, err := s.q.GetMemberByID(r.Context(), mID)
	if err != nil || m.OrganizationID != o.ID {
		http.NotFound(w, r)
		return
	}
	role := r.FormValue("role")
	if role != "owner" && role != "admin" && role != "member" {
		http.Error(w, "invalid role", http.StatusBadRequest)
		return
	}
	// Only an owner may grant the owner role (mirrors createMember, which
	// never lets a non-owner mint an owner). Prevents admins escalating
	// members — or themselves — beyond admin level.
	if role == "owner" && actorRole != "owner" {
		http.Error(w, "only an owner can grant the owner role", http.StatusForbidden)
		return
	}
	if m.Role == "owner" && role != "owner" {
		n, err := s.q.CountOwners(r.Context(), o.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n <= 1 {
			http.Error(w, "cannot demote the last owner", http.StatusForbidden)
			return
		}
	}
	if err := s.q.UpdateMemberRole(r.Context(), db.UpdateMemberRoleParams{ID: mID, Role: role}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members", http.StatusSeeOther)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	mID, ok := pathID(r, "mID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	m, err := s.q.GetMemberByID(r.Context(), mID)
	if err != nil || m.OrganizationID != o.ID {
		http.NotFound(w, r)
		return
	}
	if m.Role == "owner" {
		n, err := s.q.CountOwners(r.Context(), o.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n <= 1 {
			http.Error(w, "cannot remove the last owner", http.StatusForbidden)
			return
		}
	}
	if err := s.q.DeleteMember(r.Context(), mID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members", http.StatusSeeOther)
}
