package auth

import (
	"context"
	"fmt"

	"github.com/workos/workos-go/v4/pkg/organizations"
	"github.com/workos/workos-go/v4/pkg/usermanagement"
)

// OrgManager wraps WorkOS organization and membership SDK calls.
type OrgManager struct {
	orgClient *organizations.Client
	userMgr   *usermanagement.Client
}

// NewOrgManager creates a new OrgManager with the given WorkOS API key.
func NewOrgManager(apiKey string) *OrgManager {
	return &OrgManager{
		orgClient: &organizations.Client{APIKey: apiKey},
		userMgr:   usermanagement.NewClient(apiKey),
	}
}

// CreateOrganization creates a new WorkOS organization and returns its ID.
func (m *OrgManager) CreateOrganization(ctx context.Context, name string) (string, error) {
	org, err := m.orgClient.CreateOrganization(ctx, organizations.CreateOrganizationOpts{
		Name: name,
	})
	if err != nil {
		return "", fmt.Errorf("workos: failed to create organization: %w", err)
	}
	return org.ID, nil
}

// UpdateOrganization renames a WorkOS organization.
func (m *OrgManager) UpdateOrganization(ctx context.Context, orgID, name string) error {
	_, err := m.orgClient.UpdateOrganization(ctx, organizations.UpdateOrganizationOpts{
		Organization: orgID,
		Name:         name,
	})
	if err != nil {
		return fmt.Errorf("workos: failed to update organization: %w", err)
	}
	return nil
}

// GetOrganization returns a WorkOS organization by ID.
func (m *OrgManager) GetOrganization(ctx context.Context, orgID string) (*organizations.Organization, error) {
	org, err := m.orgClient.GetOrganization(ctx, organizations.GetOrganizationOpts{
		Organization: orgID,
	})
	if err != nil {
		return nil, fmt.Errorf("workos: failed to get organization: %w", err)
	}
	return &org, nil
}

// CreateMembership adds a user to a WorkOS organization.
func (m *OrgManager) CreateMembership(ctx context.Context, workosOrgID, workosUserID string, roleSlug string) (string, error) {
	membership, err := m.userMgr.CreateOrganizationMembership(ctx, usermanagement.CreateOrganizationMembershipOpts{
		UserID:         workosUserID,
		OrganizationID: workosOrgID,
		RoleSlug:       roleSlug,
	})
	if err != nil {
		return "", fmt.Errorf("workos: failed to create membership: %w", err)
	}
	return membership.ID, nil
}

// ListMemberships returns all memberships for a WorkOS organization.
func (m *OrgManager) ListMemberships(ctx context.Context, workosOrgID string) ([]usermanagement.OrganizationMembership, error) {
	var all []usermanagement.OrganizationMembership
	var after string
	for {
		resp, err := m.userMgr.ListOrganizationMemberships(ctx, usermanagement.ListOrganizationMembershipsOpts{
			OrganizationID: workosOrgID,
			Limit:          100,
			After:          after,
		})
		if err != nil {
			return nil, fmt.Errorf("workos: failed to list memberships: %w", err)
		}
		all = append(all, resp.Data...)
		if resp.ListMetadata.After == "" {
			break
		}
		after = resp.ListMetadata.After
	}
	return all, nil
}

// ListUserMemberships returns all org memberships for a specific WorkOS user.
func (m *OrgManager) ListUserMemberships(ctx context.Context, workosUserID string) ([]usermanagement.OrganizationMembership, error) {
	var all []usermanagement.OrganizationMembership
	var after string
	for {
		resp, err := m.userMgr.ListOrganizationMemberships(ctx, usermanagement.ListOrganizationMembershipsOpts{
			UserID: workosUserID,
			Limit:  100,
			After:  after,
		})
		if err != nil {
			return nil, fmt.Errorf("workos: failed to list user memberships: %w", err)
		}
		all = append(all, resp.Data...)
		if resp.ListMetadata.After == "" {
			break
		}
		after = resp.ListMetadata.After
	}
	return all, nil
}

// DeleteMembership removes a user from a WorkOS organization.
func (m *OrgManager) DeleteMembership(ctx context.Context, membershipID string) error {
	err := m.userMgr.DeleteOrganizationMembership(ctx, usermanagement.DeleteOrganizationMembershipOpts{
		OrganizationMembership: membershipID,
	})
	if err != nil {
		return fmt.Errorf("workos: failed to delete membership: %w", err)
	}
	return nil
}

// SendInvitation sends an invitation to join a WorkOS organization.
func (m *OrgManager) SendInvitation(ctx context.Context, email, workosOrgID, inviterUserID, roleSlug string, expiresInDays int) (*usermanagement.Invitation, error) {
	inv, err := m.userMgr.SendInvitation(ctx, usermanagement.SendInvitationOpts{
		Email:          email,
		OrganizationID: workosOrgID,
		InviterUserID:  inviterUserID,
		RoleSlug:       roleSlug,
		ExpiresInDays:  expiresInDays,
	})
	if err != nil {
		return nil, fmt.Errorf("workos: failed to send invitation: %w", err)
	}
	return &inv, nil
}

// ListInvitations returns pending invitations for a WorkOS organization.
func (m *OrgManager) ListInvitations(ctx context.Context, workosOrgID string) ([]usermanagement.Invitation, error) {
	var all []usermanagement.Invitation
	var after string
	for {
		resp, err := m.userMgr.ListInvitations(ctx, usermanagement.ListInvitationsOpts{
			OrganizationID: workosOrgID,
			Limit:          100,
			After:          after,
		})
		if err != nil {
			return nil, fmt.Errorf("workos: failed to list invitations: %w", err)
		}
		all = append(all, resp.Data...)
		if resp.ListMetadata.After == "" {
			break
		}
		after = resp.ListMetadata.After
	}
	return all, nil
}

// RevokeInvitation revokes a pending WorkOS invitation.
func (m *OrgManager) RevokeInvitation(ctx context.Context, invitationID string) error {
	_, err := m.userMgr.RevokeInvitation(ctx, usermanagement.RevokeInvitationOpts{
		Invitation: invitationID,
	})
	if err != nil {
		return fmt.Errorf("workos: failed to revoke invitation: %w", err)
	}
	return nil
}

// GetUser returns a WorkOS user by ID.
func (m *OrgManager) GetUser(ctx context.Context, userID string) (*usermanagement.User, error) {
	user, err := m.userMgr.GetUser(ctx, usermanagement.GetUserOpts{
		User: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("workos: failed to get user: %w", err)
	}
	return &user, nil
}
