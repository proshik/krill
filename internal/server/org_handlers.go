package server

import (
	"net/http"
	"strconv"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// home redirects to the org list.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/orgs", http.StatusSeeOther)
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.q.ListOrganizationsForUser(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		logFrom(r).Error("listOrgs: failed to list organizations", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.Orgs(orgs))
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	o, err := s.org.CreateOrg(r.Context(), auth.UserID(r.Context()), name)
	if err != nil {
		logFrom(r).Error("createOrg: failed to create organization", "err", err, "name", name)
		s.flashErr(w, r, "failed to create organization: "+err.Error())
		return
	}
	logFrom(r).Info("organization created", "org_id", o.ID, "name", o.Name)
	s.setFlash(w, "ok", "Organization created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
}

func (s *Server) orgDashboard(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	projects, err := s.q.ListProjects(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("orgDashboard: failed to list projects", "err", err, "org_id", o.ID)
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
		logFrom(r).Error("listMembers: failed to list members", "err", err, "org_id", o.ID)
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
	// create a user with a temporary password (or find an existing one)
	tempPw, err := auth.NewToken()
	if err != nil {
		logFrom(r).Error("createMember: failed to generate temporary password", "err", err, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	tempPw = tempPw[:12]
	u, err := s.q.GetUserByEmail(r.Context(), email)
	if err != nil {
		hash, herr := auth.HashPassword(tempPw)
		if herr != nil {
			logFrom(r).Error("createMember: failed to hash password", "err", herr, "org_id", o.ID)
			s.flashErr(w, r, herr.Error())
			return
		}
		u, err = s.q.CreateUser(r.Context(), db.CreateUserParams{Email: email, PasswordHash: hash})
		if err != nil {
			logFrom(r).Error("createMember: failed to create user", "err", err, "org_id", o.ID)
			s.flashErr(w, r, "failed to create user: "+err.Error())
			return
		}
	} else {
		tempPw = "" // existing user — don't show the password
	}
	if _, err := s.q.CreateMember(r.Context(), db.CreateMemberParams{
		OrganizationID: o.ID, UserID: u.ID, Role: role,
	}); err != nil {
		logFrom(r).Error("createMember: failed to add member", "err", err, "org_id", o.ID, "user_id", u.ID, "role", role)
		s.flashErr(w, r, "user already in the organization?")
		return
	}
	if tempPw != "" {
		s.setFlashPw(w, tempPw)
	}
	logFrom(r).Info("member added", "org_id", o.ID, "user_id", u.ID, "role", role)
	s.setFlash(w, "ok", "Member added")
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
		logFrom(r).Info("updateMemberRole: member not found or org mismatch", "member_id", mID, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	role := r.FormValue("role")
	if role != "owner" && role != "admin" && role != "member" {
		s.flashErr(w, r, "invalid role")
		return
	}
	// Only an owner may grant the owner role (mirrors createMember, which
	// never lets a non-owner mint an owner). Prevents admins escalating
	// members — or themselves — beyond admin level.
	if role == "owner" && actorRole != "owner" {
		logFrom(r).Warn("updateMemberRole: non-owner attempted to grant owner role", "member_id", mID, "org_id", o.ID, "role", actorRole)
		s.flashErr(w, r, "only an owner can grant the owner role")
		return
	}
	if m.Role == "owner" && role != "owner" {
		n, err := s.q.CountOwners(r.Context(), o.ID)
		if err != nil {
			logFrom(r).Error("updateMemberRole: failed to count owners", "err", err, "org_id", o.ID)
			s.flashErr(w, r, err.Error())
			return
		}
		if n <= 1 {
			logFrom(r).Warn("updateMemberRole: attempt to demote the last owner", "member_id", mID, "org_id", o.ID)
			s.flashErr(w, r, "cannot demote the last owner")
			return
		}
	}
	if err := s.q.UpdateMemberRole(r.Context(), db.UpdateMemberRoleParams{ID: mID, Role: role}); err != nil {
		logFrom(r).Error("updateMemberRole: failed to update member role", "err", err, "member_id", mID, "org_id", o.ID, "role", role)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("member role changed", "member_id", mID, "org_id", o.ID, "role", role)
	s.setFlash(w, "ok", "Role updated")
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
		logFrom(r).Info("removeMember: member not found or org mismatch", "member_id", mID, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	if m.Role == "owner" {
		n, err := s.q.CountOwners(r.Context(), o.ID)
		if err != nil {
			logFrom(r).Error("removeMember: failed to count owners", "err", err, "org_id", o.ID)
			s.flashErr(w, r, err.Error())
			return
		}
		if n <= 1 {
			logFrom(r).Warn("removeMember: attempt to remove the last owner", "member_id", mID, "org_id", o.ID)
			s.flashErr(w, r, "cannot remove the last owner")
			return
		}
	}
	if err := s.q.DeleteMember(r.Context(), mID); err != nil {
		logFrom(r).Error("removeMember: failed to delete member", "err", err, "member_id", mID, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("member removed", "member_id", mID, "org_id", o.ID)
	s.setFlash(w, "ok", "Member removed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members", http.StatusSeeOther)
}
