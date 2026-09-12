package server

import (
	"net/http"
	"strconv"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/orgnet"
	"github.com/proshik/krill/internal/web/templates"
)

// home sends the user into their first organization's dashboard (the app shell),
// so opening the app lands on the real interface rather than the standalone org
// list. Only when the user has no organization yet do we fall back to /orgs to
// create one.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.q.ListOrganizationsForUser(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		logFrom(r).Error("home: failed to list organizations", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(orgs) == 0 {
		http.Redirect(w, r, "/orgs", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(orgs[0].ID, 10), http.StatusSeeOther)
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

// orgSwitcher renders the sidebar org-switcher modal body: the user's
// organizations (the one named by the "current" query param highlighted) plus a
// create form. It is an HTMX partial loaded into the #org-switcher dialog.
func (s *Server) orgSwitcher(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.q.ListOrganizationsForUser(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		logFrom(r).Error("orgSwitcher: failed to list organizations", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	current, _ := strconv.ParseInt(r.URL.Query().Get("current"), 10, 64)
	render(w, r, http.StatusOK, templates.OrgSwitcher(orgs, current))
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	o, err := s.org.CreateOrg(r.Context(), auth.UserID(r.Context()), name)
	if err != nil {
		logFrom(r).Error("createOrg: failed to create organization", "err", err, "name", name)
		s.flashErrErr(w, r, "flash.err.create_org", err)
		return
	}
	logFrom(r).Info("organization created", "org_id", o.ID, "name", o.Name)

	// Every organization gets its own overlay network, so its apps and DB
	// instances never resolve or reach another tenant's services by name.
	// A half-made organization (a row with no working network) must not
	// survive: an org that can never deploy anything is worse than none.
	netName := orgnet.Name(o.ID)
	if _, err := s.engine.NetworkEnsure(r.Context(), netName); err != nil {
		logFrom(r).Error("could not create the organization network", "err", err, "org_id", o.ID)
		if derr := s.q.DeleteOrganization(r.Context(), o.ID); derr != nil {
			logFrom(r).Error("could not roll back an organization left without a network", "err", derr, "org_id", o.ID)
		}
		s.flashErrT(w, r, "flash.err.org_network")
		http.Redirect(w, r, "/orgs", http.StatusSeeOther)
		return
	}
	if err := s.q.SetOrganizationNetwork(r.Context(), db.SetOrganizationNetworkParams{ID: o.ID, NetworkName: netName}); err != nil {
		logFrom(r).Error("could not store the organization network", "err", err, "org_id", o.ID)
	}

	// The gateway lives in every organization network; until it joins this one,
	// nothing deployed here is reachable from outside. The reconcile is
	// requested, not performed: it recreates the Traefik task and rebinds
	// :80/:443, and creating an organization needs no role — doing it inline
	// would let a loop of POST /orgs hold ingress down for every tenant.
	if s.reconcileGateway != nil {
		s.reconcileGateway()
	}

	s.flashOK(w, r, "flash.ok.org_created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
}

func (s *Server) orgDashboard(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	rows, err := s.q.ListProjectsWithCounts(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("orgDashboard: failed to list projects", "err", err, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cards := make([]templates.ProjectCard, 0, len(rows))
	for _, p := range rows {
		cards = append(cards, templates.ProjectCard{
			Project: db.Project{
				ID: p.ID, OrganizationID: p.OrganizationID, Name: p.Name,
				Slug: p.Slug, Description: p.Description, CreatedAt: p.CreatedAt,
			},
			EnvCount: p.EnvCount,
			AppCount: p.AppCount,
		})
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
			s.flashErrErr(w, r, "flash.err.create_user", err)
			return
		}
		// The inviter chose this temporary password and keeps a working
		// credential for the account until the invitee changes it — hold them
		// on the change-password form until they do.
		if err := s.q.SetUserMustChangePassword(r.Context(), db.SetUserMustChangePasswordParams{ID: u.ID, MustChangePassword: true}); err != nil {
			logFrom(r).Error("could not flag the invited user for a password change", "err", err, "user_id", u.ID)
		}
	} else {
		tempPw = "" // existing user — don't show the password
	}
	if _, err := s.q.CreateMember(r.Context(), db.CreateMemberParams{
		OrganizationID: o.ID, UserID: u.ID, Role: role,
	}); err != nil {
		logFrom(r).Error("createMember: failed to add member", "err", err, "org_id", o.ID, "user_id", u.ID, "role", role)
		s.flashErrT(w, r, "flash.err.user_in_org")
		return
	}
	if tempPw != "" {
		s.setFlashPw(w, tempPw)
	}
	logFrom(r).Info("member added", "org_id", o.ID, "user_id", u.ID, "role", role)
	s.flashOK(w, r, "flash.ok.member_added")
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
		s.flashErrT(w, r, "flash.err.invalid_role")
		return
	}
	// Only an owner may grant the owner role (mirrors createMember, which
	// never lets a non-owner mint an owner). Prevents admins escalating
	// members — or themselves — beyond admin level.
	if role == "owner" && actorRole != "owner" {
		logFrom(r).Warn("updateMemberRole: non-owner attempted to grant owner role", "member_id", mID, "org_id", o.ID, "role", actorRole)
		s.flashErrT(w, r, "flash.err.owner_grant_owner")
		return
	}
	// An actor may not act on a member who outranks them: an admin must not be
	// able to demote (or, in removeMember, delete) an owner.
	if m.Role == "owner" && actorRole != "owner" {
		logFrom(r).Warn("updateMemberRole: non-owner attempted to change an owner's role", "member_id", mID, "org_id", o.ID, "role", actorRole)
		s.flashErrT(w, r, "flash.err.owner_change_owner")
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
			s.flashErrT(w, r, "flash.err.last_owner_demote")
			return
		}
	}
	if err := s.q.UpdateMemberRole(r.Context(), db.UpdateMemberRoleParams{ID: mID, Role: role}); err != nil {
		logFrom(r).Error("updateMemberRole: failed to update member role", "err", err, "member_id", mID, "org_id", o.ID, "role", role)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("member role changed", "member_id", mID, "org_id", o.ID, "role", role)
	s.flashOK(w, r, "flash.ok.role_updated")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members", http.StatusSeeOther)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
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
		logFrom(r).Info("removeMember: member not found or org mismatch", "member_id", mID, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	// An actor may not remove a member who outranks them: an admin must not be
	// able to remove an owner (mirrors the updateMemberRole guard).
	if m.Role == "owner" && actorRole != "owner" {
		logFrom(r).Warn("removeMember: non-owner attempted to remove an owner", "member_id", mID, "org_id", o.ID, "role", actorRole)
		s.flashErrT(w, r, "flash.err.owner_remove_owner")
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
			s.flashErrT(w, r, "flash.err.last_owner_remove")
			return
		}
	}
	if err := s.q.DeleteMember(r.Context(), mID); err != nil {
		logFrom(r).Error("removeMember: failed to delete member", "err", err, "member_id", mID, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("member removed", "member_id", mID, "org_id", o.ID)
	s.flashOK(w, r, "flash.ok.member_removed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members", http.StatusSeeOther)
}
