package google

import (
	"reflect"
	"testing"

	admin "google.golang.org/api/admin/directory/v1"
)

func TestUserFromAdmin(t *testing.T) {
	got := userFromAdmin(&admin.User{PrimaryEmail: "alice@example.com", OrgUnitPath: "/Students/HS"})
	want := User{Email: "alice@example.com", OrgUnitPath: "/Students/HS"}
	if got != want {
		t.Fatalf("userFromAdmin = %+v, want %+v", got, want)
	}
}

func TestSerializeDeserializeUsersRoundTrip(t *testing.T) {
	in := []User{
		{Email: "a@example.com", OrgUnitPath: "/Students"},
		{Email: "b@example.com", OrgUnitPath: "/Faculty/Adjuncts"},
	}
	data, err := SerializeUsers(in)
	if err != nil {
		t.Fatalf("SerializeUsers: %v", err)
	}
	out, err := DeserializeUsers(data)
	if err != nil {
		t.Fatalf("DeserializeUsers: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip = %+v, want %+v", out, in)
	}
}
