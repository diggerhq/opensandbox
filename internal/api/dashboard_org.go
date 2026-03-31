package api

import (
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
)

// dashboardListOrgMembers returns the members of the current org via WorkOS.
func (s *Server) dashboardListOrgMembers(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "org not found"})
	}

	if org.WorkOSOrgID == nil {
		// Org not linked to WorkOS yet — return local users only
		users, err := s.store.ListUsersByOrgID(c.Request().Context(), orgID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		type memberResp struct {
			ID    uuid.UUID `json:"id"`
			Email string    `json:"email"`
			Name  string    `json:"name"`
			Role  string    `json:"role"`
		}
		var members []memberResp
		for _, u := range users {
			members = append(members, memberResp{
				ID:    u.ID,
				Email: u.Email,
				Name:  u.Name,
				Role:  u.Role,
			})
		}
		return c.JSON(http.StatusOK, members)
	}

	// Fetch from WorkOS
	memberships, err := s.workos.OrgMgr().ListMemberships(c.Request().Context(), *org.WorkOSOrgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	type memberResp struct {
		MembershipID string `json:"membershipId"`
		UserID       string `json:"workosUserId"`
		Email        string `json:"email"`
		Name         string `json:"name"`
		Role         string `json:"role"`
		Status       string `json:"status"`
	}

	var members []memberResp
	for _, m := range memberships {
		// Enrich with user details from WorkOS
		email := ""
		name := ""
		workosUser, err := s.workos.OrgMgr().GetUser(c.Request().Context(), m.UserID)
		if err == nil {
			email = workosUser.Email
			name = workosUser.FirstName
			if workosUser.LastName != "" {
				name += " " + workosUser.LastName
			}
		}
		members = append(members, memberResp{
			MembershipID: m.ID,
			UserID:       m.UserID,
			Email:        email,
			Name:         name,
			Role:         m.Role.Slug,
			Status:       string(m.Status),
		})
	}

	return c.JSON(http.StatusOK, members)
}

// dashboardRemoveMember removes a member from the org via WorkOS.
func (s *Server) dashboardRemoveMember(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	membershipID := c.Param("membershipId")
	if membershipID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "membershipId required"})
	}

	err := s.workos.OrgMgr().DeleteMembership(c.Request().Context(), membershipID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "removed"})
}

// dashboardSendInvitation sends an invitation to join the current org.
func (s *Server) dashboardSendInvitation(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Email == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "email is required"})
	}
	if req.Role == "" {
		req.Role = "member"
	}

	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "org not found"})
	}
	if org.WorkOSOrgID == nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "org not linked to WorkOS — run backfill first"})
	}

	// Get inviter's WorkOS user ID
	inviterWorkOSUserID := ""
	if uid, ok := c.Get("user_id").(uuid.UUID); ok {
		user, err := s.store.GetUserByEmail(c.Request().Context(), c.Get("user_email").(string))
		if err == nil && user.WorkOSUserID != nil {
			inviterWorkOSUserID = *user.WorkOSUserID
		}
		_ = uid
	}

	inv, err := s.workos.OrgMgr().SendInvitation(c.Request().Context(), req.Email, *org.WorkOSOrgID, inviterWorkOSUserID, req.Role, 7)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":        inv.ID,
		"email":     inv.Email,
		"state":     inv.State,
		"expiresAt": inv.ExpiresAt,
		"createdAt": inv.CreatedAt,
	})
}

// dashboardListInvitations returns pending invitations for the current org.
func (s *Server) dashboardListInvitations(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "org not found"})
	}
	if org.WorkOSOrgID == nil {
		return c.JSON(http.StatusOK, []interface{}{})
	}

	invitations, err := s.workos.OrgMgr().ListInvitations(c.Request().Context(), *org.WorkOSOrgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	type invResp struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		State     string `json:"state"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expiresAt"`
		CreatedAt string `json:"createdAt"`
	}

	var resp []invResp
	for _, inv := range invitations {
		resp = append(resp, invResp{
			ID:        inv.ID,
			Email:     inv.Email,
			State:     string(inv.State),
			ExpiresAt: inv.ExpiresAt,
			CreatedAt: inv.CreatedAt,
		})
	}

	return c.JSON(http.StatusOK, resp)
}

// dashboardRevokeInvitation revokes a pending invitation.
func (s *Server) dashboardRevokeInvitation(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	invID := c.Param("id")
	if invID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invitation id required"})
	}

	err := s.workos.OrgMgr().RevokeInvitation(c.Request().Context(), invID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "revoked"})
}

// dashboardListOrgs returns all orgs the user belongs to (via WorkOS memberships).
func (s *Server) dashboardListOrgs(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	email, ok := c.Get("user_email").(string)
	if !ok || email == "" {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	user, err := s.store.GetUserByEmail(c.Request().Context(), email)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}

	if user.WorkOSUserID == nil {
		// No WorkOS link — return only the current org
		org, err := s.store.GetOrg(c.Request().Context(), user.OrgID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		type orgResp struct {
			ID         uuid.UUID `json:"id"`
			Name       string    `json:"name"`
			IsPersonal bool      `json:"isPersonal"`
			IsActive   bool      `json:"isActive"`
		}
		return c.JSON(http.StatusOK, []orgResp{{
			ID:         org.ID,
			Name:       org.Name,
			IsPersonal: org.IsPersonal,
			IsActive:   true,
		}})
	}

	memberships, err := s.workos.OrgMgr().ListUserMemberships(c.Request().Context(), *user.WorkOSUserID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	type orgResp struct {
		ID         uuid.UUID `json:"id"`
		Name       string    `json:"name"`
		IsPersonal bool      `json:"isPersonal"`
		IsActive   bool      `json:"isActive"`
	}

	var orgs []orgResp
	for _, m := range memberships {
		localOrg, err := s.store.GetOrgByWorkOSID(c.Request().Context(), m.OrganizationID)
		if err != nil {
			continue
		}
		orgs = append(orgs, orgResp{
			ID:         localOrg.ID,
			Name:       localOrg.Name,
			IsPersonal: localOrg.IsPersonal,
			IsActive:   localOrg.ID == user.OrgID,
		})
	}

	return c.JSON(http.StatusOK, orgs)
}

// dashboardSwitchOrg switches the user's active org.
func (s *Server) dashboardSwitchOrg(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	var req struct {
		OrgID uuid.UUID `json:"orgId"`
	}
	if err := c.Bind(&req); err != nil || req.OrgID == uuid.Nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "orgId is required"})
	}

	email, ok := c.Get("user_email").(string)
	if !ok || email == "" {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	user, err := s.store.GetUserByEmail(c.Request().Context(), email)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}

	// Validate the user has access to the target org via WorkOS
	if s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "identity provider not configured"})
	}
	if user.WorkOSUserID == nil || *user.WorkOSUserID == "" {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "user account not linked to identity provider"})
	}
	targetOrg, err := s.store.GetOrg(c.Request().Context(), req.OrgID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "target org not found"})
	}
	if targetOrg.WorkOSOrgID == nil || *targetOrg.WorkOSOrgID == "" {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "target org not linked to identity provider"})
	}
	memberships, err := s.workos.OrgMgr().ListUserMemberships(c.Request().Context(), *user.WorkOSUserID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	found := false
	for _, m := range memberships {
		if m.OrganizationID == *targetOrg.WorkOSOrgID {
			found = true
			break
		}
	}
	if !found {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "not a member of this org"})
	}

	if err := s.store.SetActiveOrg(c.Request().Context(), user.ID, req.OrgID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	org, err := s.store.GetOrg(c.Request().Context(), req.OrgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, org)
}

// dashboardGetCredits returns the credit balance for the current org.
func (s *Server) dashboardGetCredits(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "org not found"})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"balanceCents": org.CreditBalanceCents,
		"isPersonal":   org.IsPersonal,
	})
}

// dashboardBackfillWorkOSOrgs is a one-time admin endpoint to create WorkOS orgs
// for existing orgs that predate the WorkOS integration.
func (s *Server) dashboardBackfillWorkOSOrgs(c echo.Context) error {
	if s.store == nil || s.workos == nil || s.workos.OrgMgr() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	ctx := c.Request().Context()
	orgs, err := s.store.ListOrgsWithoutWorkOS(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	type result struct {
		OrgID      uuid.UUID `json:"orgId"`
		OrgName    string    `json:"orgName"`
		WorkOSOrgID string   `json:"workosOrgId,omitempty"`
		Error      string    `json:"error,omitempty"`
	}

	var results []result
	for _, org := range orgs {
		workosOrgID, err := s.workos.OrgMgr().CreateOrganization(ctx, org.Name)
		if err != nil {
			log.Printf("backfill: failed to create WorkOS org for %s: %v", org.Name, err)
			results = append(results, result{OrgID: org.ID, OrgName: org.Name, Error: err.Error()})
			continue
		}

		if err := s.store.UpdateOrgWorkOSID(ctx, org.ID, workosOrgID); err != nil {
			log.Printf("backfill: failed to update org %s with WorkOS ID: %v", org.Name, err)
			results = append(results, result{OrgID: org.ID, OrgName: org.Name, Error: err.Error()})
			continue
		}

		// Create WorkOS memberships for all users in this org
		users, err := s.store.ListUsersByOrgID(ctx, org.ID)
		if err == nil {
			for _, user := range users {
				if user.WorkOSUserID != nil {
					_, err := s.workos.OrgMgr().CreateMembership(ctx, workosOrgID, *user.WorkOSUserID, "admin")
					if err != nil {
						log.Printf("backfill: failed to create membership for user %s in org %s: %v", user.Email, org.Name, err)
					}
				}
			}
		}

		log.Printf("backfill: created WorkOS org %s for %s", workosOrgID, org.Name)
		results = append(results, result{OrgID: org.ID, OrgName: org.Name, WorkOSOrgID: workosOrgID})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"processed": len(results),
		"results":   results,
	})
}
