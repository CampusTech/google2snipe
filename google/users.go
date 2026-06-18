package google

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"
)

// User is a Directory user reduced to the fields the license OU filter needs.
type User struct {
	Email       string `json:"email"`
	OrgUnitPath string `json:"org_unit_path"`
}

// userFromAdmin reduces an Admin SDK user to the fields we keep.
func userFromAdmin(u *admin.User) User {
	return User{Email: u.PrimaryEmail, OrgUnitPath: u.OrgUnitPath}
}

// ListAllUsers pages through every Directory user for the customer, returning each
// user's primary email and org unit path. It requests only those two fields and
// includes suspended users (a suspended account can still hold a license). Requires
// the admin.directory.user.readonly scope (config google.scopes + DWD grant).
func (c *Client) ListAllUsers(ctx context.Context) ([]User, error) {
	customer := c.customerID
	if customer == "" {
		customer = "my_customer"
	}
	var out []User
	pageToken := ""
	for {
		call := c.svc.Users.List().
			Customer(customer).
			MaxResults(500).
			Fields("nextPageToken,users(primaryEmail,orgUnitPath)").
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list directory users (needs admin.directory.user.readonly scope): %w", err)
		}
		for _, u := range resp.Users {
			out = append(out, userFromAdmin(u))
		}
		c.log.WithFields(logrus.Fields{"page": len(resp.Users), "total": len(out)}).Debug("listed directory users page")
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// SerializeUsers marshals users to indented JSON for caching.
func SerializeUsers(users []User) ([]byte, error) {
	return json.MarshalIndent(users, "", "  ")
}

// DeserializeUsers reads cached JSON back into users.
func DeserializeUsers(data []byte) ([]User, error) {
	var users []User
	return users, json.Unmarshal(data, &users)
}
