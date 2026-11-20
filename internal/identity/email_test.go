package identity

import "testing"

// LLDAP enforces uniqueness on the lowercased email, so redemption must detect
// a collision the same way. Before this, a second invite sharing an address
// surfaced the raw database error "UNIQUE constraint failed:
// users.lowercase_email" to the invitee.
func TestSameEmailMatchesTheWayLLDAPDoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a, b   string
		wanted bool
	}{
		{"exact", "someone@example.com", "someone@example.com", true},
		{"case differs", "Someone@Example.com", "someone@example.com", true},
		{"padded", "  someone@example.com  ", "someone@example.com", true},
		{"different", "other@example.com", "someone@example.com", false},
		{"one empty", "", "someone@example.com", false},
		{"both empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameEmail(tc.a, tc.b); got != tc.wanted {
				t.Fatalf("sameEmail(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.wanted)
			}
		})
	}
}
